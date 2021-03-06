package lib

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
)

// Debugf function, if the debug flag is set, then display. Do nothing otherwise
// If Docker is in damon mode, also send the debug info on the socket
// Convenience debug function, courtesy of http://github.com/dotcloud/docker
func Debugf(format string, a ...interface{}) {
	if GlobalConfig.Debug || os.Getenv("DEBUG") != "" {

		// Retrieve the stack infos
		_, file, line, ok := runtime.Caller(1)
		if !ok {
			file = "<unknown>"
			line = -1
		} else {
			file = file[strings.LastIndex(file, "/")+1:]
		}

		log.Printf(fmt.Sprintf("[%d] [debug] %s:%d %s\n", os.Getpid(), file, line, format), a...)
	}
}
