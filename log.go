// Copyright 2013 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package daemon

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
)

var (
	logPrefix = fmt.Sprintf("[%d] ", os.Getpid())
	logFlags  = log.Ldate | log.Lmicroseconds | log.Lshortfile
	logFile   = os.Stderr
	logger    = log.New(logFile, logPrefix, logFlags)
)

// A Logger is a level-filtered log writer.
type Logger int

// Default log levels.  Some of these levels have special meanings; see the
// documentation for Printf.
//
// In the documentation for this package, "higher" log levels correspond to
// lower numeric values in this list; that is, Error is a higher log level
// than Verbose.
const (
	Error Logger = iota
	Warning
	Info
	Verbose

	Exit  Logger = -1
	Fatal Logger = -2
)

// V returns a verbose logger at the given level.  This should
// generally be 3 or higher, to avoid collisions with the standard
// log levels.  By default, these will be suppressed unless LogLevel
// is set or a LogLevelFlag is registered.
func V(level int) Logger {
	return Logger(level)
}

// LogLevel controls what log messages are written to the log.  Only logs
// destined for an equal or higher level will be written.
var LogLevel = Info

func (l Logger) prefix() string {
	switch l {
	case Error, Fatal:
		return "E: "
	case Warning:
		return "W: "
	case Info:
		return "I: "
	}
	return "V: "
}

func stack() string {
	n, stack := 0, make([]byte, 4096)
	for i := 0; i < 10; i++ {
		n = runtime.Stack(stack, true)
		if n < len(stack) {
			break
		}
		stack = make([]byte, len(stack)*2)
	}
	if n == len(stack) {
		stack = append(stack, "..."...)
	} else {
		stack = stack[:n]
	}
	return string(stack)
}

// Printf formats the log message and writes it to the log if the level is
// sufficient.  If the message is directed at Exit or Fatal, the binary will
// terminate after the log message is written.  If the message is directed to
// Fatal or lower, a stack trace of all goroutines will also be written to the
// log before exiting.  If the logger is Warning or higher, the log will be
// Sync'd after writing.
func (l Logger) Printf(format string, args ...interface{}) {
	if l > LogLevel {
		return
	}
	msg := fmt.Sprintf(l.prefix()+format, args...)
	if l <= Fatal {
		msg += "\n" + stack()
	}
	logger.Output(2, msg)
	if l < Info {
		logFile.Sync()
	}
	if l == Exit || l == Fatal {
		os.Exit(1)
	}
}

// LogLevelFlag registers a flag with the given name which, when set, causes
// only log messages of equal or higher level to be logged.  A pointer to the
// log level chosen is returned.
func LogLevelFlag(name string) *Logger {
	flag.IntVar((*int)(&LogLevel), name, int(LogLevel), "Log level (0=Error, 1=Warning, 2=Info, 3+Verbose)")
	return &LogLevel
}

type logFileFlag struct {
	mode os.FileMode
}

func (f *logFileFlag) String() string {
	return logFile.Name()
}

func (f *logFileFlag) Set(s string) error {
	file, err := os.OpenFile(s, os.O_WRONLY|os.O_APPEND|os.O_CREATE, f.mode)
	if err != nil {
		return err
	}
	logger = log.New(io.MultiWriter(os.Stderr, file), logPrefix, logFlags)
	logFile = file
	redirectStdout() // provided in OS-specific files
	return nil
}

// LogFileFlag registers a flag with the given name which, when set,
// causes daemon logs to be sent to the given file in addition to
// standard error.  A pointer to the file is also returned,
// which can be used for a deferred Close in main.
func LogFileFlag(name string, mode os.FileMode) **os.File {
	fileFlag := &logFileFlag{
		mode: mode,
	}
	flag.Var(fileFlag, name, "Log file (also writes to stderr if set)")
	return &logFile
}
