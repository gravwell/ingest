/*************************************************************************
 * Copyright 2017 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/

package ingest

import (
	"fmt"
	"strings"
	"time"

	"gravwell/oss/ingest/entry"
)

//gravwell log level type
type gll int

const (
	gravwellError gll = 3
	gravwellWarn  gll = 2
	gravwellInfo  gll = 1
	gravwellOff   gll = 0

	defaultLogLevel = gravwellError
)

type IngestLogger interface {
	Error(string, ...interface{}) error
	Warn(string, ...interface{}) error
	Info(string, ...interface{}) error
}

// GravwellError send an error entry down the line with the gravwell tag
func (im *IngestMuxer) Error(format string, args ...interface{}) error {
	if im.logLevel < gravwellError {
		return nil
	}
	return im.gravwellWrite(gravwellError, fmt.Sprintf(format, args...))
}

func (im *IngestMuxer) Warn(format string, args ...interface{}) error {
	if im.logLevel < gravwellWarn {
		return nil
	}
	return im.gravwellWrite(gravwellError, fmt.Sprintf(format, args...))
}

func (im *IngestMuxer) Info(format string, args ...interface{}) error {
	if im.logLevel < gravwellInfo {
		return nil
	}
	return im.gravwellWrite(gravwellWarn, fmt.Sprintf(format, args...))
}

func (im *IngestMuxer) gravwellWrite(level gll, line string) error {
	ts := entry.Now()
	e := &entry.Entry{
		Data: []byte(ts.Format(time.RFC3339) + ` ` + level.String() + ` ` + line),
		TS:   ts,
		Tag:  entry.GravwellTagId,
	}
	return im.WriteEntry(e)
}

func (g gll) String() string {
	switch g {
	case gravwellOff:
		return `OFF`
	case gravwellInfo:
		return `INFO`
	case gravwellWarn:
		return `WARN`
	case gravwellError:
		return `ERROR`
	}
	return `UNKNOWN`
}

func LogLevel(v string) gll {
	v = strings.TrimSpace(strings.ToUpper(v))
	if len(v) == 0 {
		return defaultLogLevel
	}
	switch v {
	case `OFF`:
		return gravwellOff
	case `INFO`:
		return gravwellInfo
	case `WARN`:
		return gravwellWarn
	case `ERROR`:
		fallthrough
	default:
		break
	}
	return gravwellError
}

type nilLogger struct{}

func (n nilLogger) Error(s string, i ...interface{}) error { return nil }
func (n nilLogger) Warn(s string, i ...interface{}) error  { return nil }
func (n nilLogger) Info(s string, i ...interface{}) error  { return nil }

func NoLogger() IngestLogger {
	return &nilLogger{}
}