/*
 *
 * k6 - a next-generation load testing tool
 * Copyright (C) 2019 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package common

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/loadimpact/k6/lib"
	"github.com/loadimpact/k6/stats"
	log "github.com/sirupsen/logrus"
)

var _ lib.Collector = &Collector{}

// Collector defines a collector struct
type Collector struct {
	Client *statsd.Client
	Config Config
	Logger *log.Entry
	Type   string
	// FilterTags will filter tags and will return a list representation of them if it's not set
	// tags are not being sent
	FilterTags func(map[string]string) []string

	startTime  time.Time
	buffer     []*Sample
	bufferLock sync.Mutex
}

// Init sets up the collector
func (c *Collector) Init() (err error) {
	if address := c.Config.Addr.String; address == "" {
		err = fmt.Errorf(
			"%s: connection string is invalid. Received: \"%+s\"",
			c.Type, address,
		)
		c.Logger.Error(err)

		return err
	}

	c.Client, err = statsd.NewBuffered(c.Config.Addr.String, int(c.Config.BufferSize.Int64))

	if err != nil {
		c.Logger.Errorf("Couldn't make buffered client, %s", err)
		return err
	}

	if namespace := c.Config.Namespace.String; namespace != "" {
		c.Client.Namespace = namespace
	}

	return nil
}

// Link returns the address of the client
func (c *Collector) Link() string {
	return c.Config.Addr.String
}

// Run the collector
func (c *Collector) Run(ctx context.Context) {
	c.Logger.Debugf("%s: Running!", c.Type)
	ticker := time.NewTicker(time.Duration(c.Config.PushInterval.Duration))
	c.startTime = time.Now()

	for {
		select {
		case <-ticker.C:
			c.pushMetrics()
		case <-ctx.Done():
			c.pushMetrics()
			c.finish()
			return
		}
	}
}

// GetRequiredSystemTags Return the required system sample tags for the specific collector
func (c *Collector) GetRequiredSystemTags() lib.TagSet {
	return lib.TagSet{} // no tags are required
}

// SetRunStatus does nothing in statsd collector
func (c *Collector) SetRunStatus(status lib.RunStatus) {}

// Collect metrics
func (c *Collector) Collect(containers []stats.SampleContainer) {
	var pointSamples []*Sample

	for _, container := range containers {
		for _, sample := range container.GetSamples() {
			pointSamples = append(pointSamples, generateDataPoint(sample))
		}
	}

	if len(pointSamples) > 0 {
		c.bufferLock.Lock()
		c.buffer = append(c.buffer, pointSamples...)
		c.bufferLock.Unlock()
	}
}

func (c *Collector) pushMetrics() {
	c.bufferLock.Lock()
	if len(c.buffer) == 0 {
		c.bufferLock.Unlock()
		return
	}
	buffer := c.buffer
	c.buffer = nil
	c.bufferLock.Unlock()

	c.Logger.
		WithField("samples", len(buffer)).
		Debugf("%s: Pushing metrics to server", c.Type)

	if err := c.commit(buffer); err != nil {
		c.Logger.
			WithError(err).
			Errorf("%s: Couldn't commit a batch", c.Type)
	}
}

func (c *Collector) finish() {
	// Close when context is done
	if err := c.Client.Close(); err != nil {
		c.Logger.Warnf("%s: Error closing the client, %+v", c.Type, err)
	}
}

func (c *Collector) commit(data []*Sample) error {
	for _, entry := range data {
		c.dispatch(entry)
	}
	return c.Client.Flush()
}

func (c *Collector) dispatch(entry *Sample) {
	var tagList []string
	if c.FilterTags != nil {
		tagList = c.FilterTags(entry.Tags)
	}

	var err error
	switch entry.Type {
	case stats.Counter:
		err = c.Client.Count(entry.Metric, int64(entry.Value), tagList, 1)
	case stats.Trend:
		err = c.Client.TimeInMilliseconds(entry.Metric, entry.Value, tagList, 1)
	case stats.Gauge:
		err = c.Client.Gauge(entry.Metric, entry.Value, tagList, 1)
	case stats.Rate:
		if check := entry.Tags["check"]; check != "" {
			err = c.Client.Count(
				checkToString(check, entry.Value),
				1,
				tagList,
				1,
			)
		} else {
			err = c.Client.Count(entry.Metric, int64(entry.Value), tagList, 1)
		}
	}
	if err != nil {
		c.Logger.Warnf("Error while sending metric %s: %s", entry.Metric, err)
	}
}

func checkToString(check string, value float64) string {
	label := "pass"
	if value == 0 {
		label = "fail"
	}
	return "check." + check + "." + label
}
