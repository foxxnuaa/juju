// Copyright 2014 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package apiserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/juju/errors"
	"github.com/juju/loggo"
	"github.com/juju/names"
	"github.com/juju/utils/tailer"
	"golang.org/x/net/websocket"

	"github.com/juju/juju/apiserver/params"
)

// debugLogHandler takes requests to watch the debug log.
type debugLogHandler struct {
	httpHandler
	logDir string
}

// ServeHTTP will serve up connections as a websocket.
// Args for the HTTP request are as follows:
//   includeEntity -> []string - lists entity tags to include in the response
//      - tags may finish with a '*' to match a prefix e.g.: unit-mysql-*, machine-2
//      - if none are set, then all lines are considered included
//   includeModule -> []string - lists logging modules to include in the response
//      - if none are set, then all lines are considered included
//   excludeEntity -> []string - lists entity tags to exclude from the response
//      - as with include, it may finish with a '*'
//   excludeModule -> []string - lists logging modules to exclude from the response
//   limit -> uint - show *at most* this many lines
//   backlog -> uint
//      - go back this many lines from the end before starting to filter
//      - has no meaning if 'replay' is true
//   level -> string one of [TRACE, DEBUG, INFO, WARNING, ERROR]
//   replay -> string - one of [true, false], if true, start the file from the start
func (h *debugLogHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	server := websocket.Server{
		Handler: func(socket *websocket.Conn) {
			logger.Infof("debug log handler starting")
			// Validate before authenticate because the authentication is
			// dependent on the state connection that is determined during the
			// validation.
			stateWrapper, err := h.validateEnvironUUID(req)
			if err != nil {
				h.sendError(socket, err)
				socket.Close()
				return
			}
			defer stateWrapper.cleanup()
			if err := stateWrapper.authenticateUser(req); err != nil {
				h.sendError(socket, fmt.Errorf("auth failed: %v", err))
				socket.Close()
				return
			}

			params, err := readDebugLogParams(req.URL.Query())
			if err != nil {
				h.sendError(socket, err)
				socket.Close()
				return
			}

			if err := h.handle(params, socket); err != nil {
				logger.Errorf("debug-log handler error: %v", err)
			}
		}}
	server.ServeHTTP(w, req)
}

// sendOk sends a nil error response, indicating there were no errors.
func (h *debugLogHandler) sendOk(w io.Writer) error {
	return h.sendError(w, nil)
}

// sendError sends a JSON-encoded error response.
func (h *debugLogHandler) sendError(w io.Writer, err error) error {
	response := &params.ErrorResult{}
	if err != nil {
		response.Error = &params.Error{Message: fmt.Sprint(err)}
	}
	message, err := json.Marshal(response)
	if err != nil {
		// If we are having trouble marshalling the error, we are in big trouble.
		logger.Errorf("failure to marshal SimpleError: %v", err)
		return err
	}
	message = append(message, []byte("\n")...)
	_, err = w.Write(message)
	return err
}

func (h *debugLogHandler) handle(params *debugLogParams, socket *websocket.Conn) error {
	stream := newLogStream(params)

	// Open log file.
	logLocation := filepath.Join(h.logDir, "all-machines.log")
	logFile, err := os.Open(logLocation)
	if err != nil {
		h.sendError(socket, fmt.Errorf("cannot open log file: %v", err))
		socket.Close()
		return err
	}
	defer logFile.Close()

	if err := stream.positionLogFile(logFile); err != nil {
		h.sendError(socket, fmt.Errorf("cannot position log file: %v", err))
		socket.Close()
		return err
	}

	// If we get to here, no more errors to report.
	if err := h.sendOk(socket); err != nil {
		socket.Close()
		return err
	}

	stream.start(logFile, socket)
	return stream.wait()
}

type debugLogParams struct {
	maxLines      uint
	fromTheStart  bool
	backlog       uint
	filterLevel   loggo.Level
	includeEntity []string
	includeModule []string
	excludeEntity []string
	excludeModule []string
}

func readDebugLogParams(queryMap url.Values) (*debugLogParams, error) {
	params := new(debugLogParams)

	if value := queryMap.Get("maxLines"); value != "" {
		num, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return nil, errors.Errorf("maxLines value %q is not a valid unsigned number", value)
		}
		params.maxLines = uint(num)
	}

	if value := queryMap.Get("replay"); value != "" {
		replay, err := strconv.ParseBool(value)
		if err != nil {
			return nil, errors.Errorf("replay value %q is not a valid boolean", value)
		}
		params.fromTheStart = replay
	}

	if value := queryMap.Get("backlog"); value != "" {
		num, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return nil, errors.Errorf("backlog value %q is not a valid unsigned number", value)
		}
		params.backlog = uint(num)
	}

	if value := queryMap.Get("level"); value != "" {
		var ok bool
		level, ok := loggo.ParseLevel(value)
		if !ok || level < loggo.TRACE || level > loggo.ERROR {
			return nil, errors.Errorf("level value %q is not one of %q, %q, %q, %q, %q",
				value, loggo.TRACE, loggo.DEBUG, loggo.INFO, loggo.WARNING, loggo.ERROR)
		}
		params.filterLevel = level
	}

	params.includeEntity = queryMap["includeEntity"]
	params.includeModule = queryMap["includeModule"]
	params.excludeEntity = queryMap["excludeEntity"]
	params.excludeModule = queryMap["excludeModule"]

	return params, nil
}

func newLogStream(params *debugLogParams) *logStream {
	return &logStream{
		debugLogParams:  params,
		maxLinesReached: make(chan bool),
	}
}

type logLine struct {
	line      string
	agentTag  string
	agentName string
	level     loggo.Level
	module    string
}

func parseLogLine(line string) *logLine {
	const (
		agentTagIndex = 0
		levelIndex    = 3
		moduleIndex   = 4
	)
	fields := strings.Fields(line)
	result := &logLine{
		line: line,
	}
	if len(fields) > agentTagIndex {
		agentTag := fields[agentTagIndex]
		// Drop mandatory trailing colon (:).
		// Since colon is mandatory, agentTag without it is invalid and will be empty ("").
		if strings.HasSuffix(agentTag, ":") {
			result.agentTag = agentTag[:len(agentTag)-1]
		}
		/*
		 Drop unit suffix.
		 In logs, unit information may be prefixed with either a unit_tag by itself or a unit_tag[nnnn].
		 The code below caters for both scenarios.
		*/
		if bracketIndex := strings.Index(agentTag, "["); bracketIndex != -1 {
			result.agentTag = agentTag[:bracketIndex]
		}
		// If, at this stage, result.agentTag is empty,  we could not deduce the tag. No point getting the name...
		if result.agentTag != "" {
			// Entity Name deduced from entity tag
			entityTag, err := names.ParseTag(result.agentTag)
			if err != nil {
				/*
				 Logging error but effectively swallowing it as there is no where to propogate.
				 We don't expect ParseTag to fail since the tag was generated by juju in the first place.
				*/
				logger.Errorf("Could not deduce name from tag %q: %v\n", result.agentTag, err)
			}
			result.agentName = entityTag.Id()
		}
	}
	if len(fields) > moduleIndex {
		if level, valid := loggo.ParseLevel(fields[levelIndex]); valid {
			result.level = level
			result.module = fields[moduleIndex]
		}
	}

	return result
}

// logStream runs the tailer to read a log file and stream
// it via a web socket.
type logStream struct {
	*debugLogParams
	logTailer       *tailer.Tailer
	lineCount       uint
	maxLinesReached chan bool
}

// positionLogFile will update the internal read position of the logFile to be
// at the end of the file or somewhere in the middle if backlog has been specified.
func (stream *logStream) positionLogFile(logFile io.ReadSeeker) error {
	// Seek to the end, or lines back from the end if we need to.
	if !stream.fromTheStart {
		return tailer.SeekLastLines(logFile, stream.backlog, stream.filterLine)
	}
	return nil
}

// start the tailer listening to the logFile, and sending the matching
// lines to the writer.
func (stream *logStream) start(logFile io.ReadSeeker, writer io.Writer) {
	stream.logTailer = tailer.NewTailer(logFile, writer, stream.countedFilterLine)
}

// wait blocks until the logTailer is done or the maximum line count
// has been reached.
func (stream *logStream) wait() error {
	select {
	case <-stream.logTailer.Dead():
		return stream.logTailer.Err()
	case <-stream.maxLinesReached:
		stream.logTailer.Stop()
	}
	return nil
}

// filterLine checks the received line for one of the configured tags.
func (stream *logStream) filterLine(line []byte) bool {
	log := parseLogLine(string(line))
	return stream.checkIncludeEntity(log) &&
		stream.checkIncludeModule(log) &&
		!stream.exclude(log) &&
		stream.checkLevel(log)
}

// countedFilterLine checks the received line for one of the configured tags,
// and also checks to make sure the stream doesn't send more than the
// specified number of lines.
func (stream *logStream) countedFilterLine(line []byte) bool {
	result := stream.filterLine(line)
	if result && stream.maxLines > 0 {
		stream.lineCount++
		result = stream.lineCount <= stream.maxLines
		if stream.lineCount == stream.maxLines {
			close(stream.maxLinesReached)
		}
	}
	return result
}

func (stream *logStream) checkIncludeEntity(line *logLine) bool {
	if len(stream.includeEntity) == 0 {
		return true
	}
	for _, value := range stream.includeEntity {
		if agentMatchesFilter(line, value) {
			return true
		}
	}
	return false
}

// agentMatchesFilter checks if agentTag tag or agentTag name match given filter
func agentMatchesFilter(line *logLine, aFilter string) bool {
	return hasMatch(line.agentName, aFilter) || hasMatch(line.agentTag, aFilter)
}

// hasMatch determines if value contains filter using regular expressions.
// All wildcard occurrences are changed to `.*`
// Currently, all match exceptions are logged and not propagated.
func hasMatch(value, aFilter string) bool {
	/* Special handling: out of 12 regexp metacharacters \^$.|?+()[*{
	   only asterix (*) can be legally used as a wildcard in this context.
	   Both machine and unit tag and name specifications do not allow any other metas.
	   Consequently, if aFilter contains wildcard (*), do not escape it -
	   transform it into a regexp "any character(s)" sequence.
	*/
	aFilter = strings.Replace(aFilter, "*", `.*`, -1)
	matches, err := regexp.MatchString("^"+aFilter+"$", value)
	if err != nil {
		// logging errors here... but really should they be swallowed?
		logger.Errorf("\nCould not match filter %q and regular expression %q\n.%v\n", value, aFilter, err)
	}
	return matches
}

func (stream *logStream) checkIncludeModule(line *logLine) bool {
	if len(stream.includeModule) == 0 {
		return true
	}
	for _, value := range stream.includeModule {
		if strings.HasPrefix(line.module, value) {
			return true
		}
	}
	return false
}

func (stream *logStream) exclude(line *logLine) bool {
	for _, value := range stream.excludeEntity {
		if agentMatchesFilter(line, value) {
			return true
		}
	}
	for _, value := range stream.excludeModule {
		if strings.HasPrefix(line.module, value) {
			return true
		}
	}
	return false
}

func (stream *logStream) checkLevel(line *logLine) bool {
	return line.level >= stream.filterLevel
}
