package register

import (
	"fmt"
	"sync"
)

type LoggerFunc func(line string)

func emitLine(printMu *sync.Mutex, logger LoggerFunc, format string, args ...any) {
	line := fmt.Sprintf(format, args...)

	printMu.Lock()
	defer printMu.Unlock()

	if logger != nil {
		logger(line)
		return
	}

	fmt.Print(line)
}
