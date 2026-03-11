package register

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/verssache/chatgpt-creator/internal/email"
	"github.com/verssache/chatgpt-creator/internal/util"
)

type BatchSummary struct {
	Target    int       `json:"target"`
	Success   int       `json:"success"`
	Attempts  int       `json:"attempts"`
	Failures  int       `json:"failures"`
	Elapsed   string    `json:"elapsed"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

// registerOne handles a single account registration.
func registerOne(workerID int, tag string, proxy, outputFile, defaultPassword, defaultDomain string, printMu, fileMu *sync.Mutex, logger LoggerFunc) (bool, string, string) {
	client, err := NewClient(proxy, tag, workerID, printMu, fileMu, logger)
	if err != nil {
		return false, "", fmt.Sprintf("failed to create client: %v", err)
	}

	emailAddr, err := email.CreateTempEmail(defaultDomain)
	if err != nil {
		return false, "", fmt.Sprintf("failed to create temp email: %v", err)
	}

	password := defaultPassword
	if password == "" {
		password = util.GeneratePassword(14)
	}

	firstName, lastName := util.RandomName()
	birthdate := util.RandomBirthdate()

	client.print(fmt.Sprintf("Starting registration for %s", emailAddr))

	err = client.RunRegister(emailAddr, password, firstName+" "+lastName, birthdate)
	if err != nil {
		return false, emailAddr, err.Error()
	}

	// Append to file
	fileMu.Lock()
	defer fileMu.Unlock()

	f, err := os.OpenFile(outputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return false, emailAddr, fmt.Sprintf("failed to open output file: %v", err)
	}
	defer f.Close()

	line := fmt.Sprintf("%s|%s\n", emailAddr, password)
	if _, err := f.WriteString(line); err != nil {
		return false, emailAddr, fmt.Sprintf("failed to write to output file: %v", err)
	}

	return true, emailAddr, ""
}

// RunBatch runs concurrent registration tasks with retry until target success count is reached.
func RunBatch(totalAccounts int, outputFile string, maxWorkers int, proxy, defaultPassword, defaultDomain string) BatchSummary {
	return RunBatchWithLogger(totalAccounts, outputFile, maxWorkers, proxy, defaultPassword, defaultDomain, nil)
}

// RunBatchWithLogger runs concurrent registration tasks and emits logs through the provided callback.
func RunBatchWithLogger(totalAccounts int, outputFile string, maxWorkers int, proxy, defaultPassword, defaultDomain string, logger LoggerFunc) BatchSummary {
	var printMu sync.Mutex
	var fileMu sync.Mutex

	var remaining int64 = int64(totalAccounts)
	var successCount int64
	var failureCount int64
	var attemptNum int64

	startTime := time.Now()

	var wg sync.WaitGroup

	for w := 1; w <= maxWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				// Claim a slot before starting work
				if atomic.AddInt64(&remaining, -1) < 0 {
					// No more slots needed, put it back and exit
					atomic.AddInt64(&remaining, 1)
					return
				}

				attempt := atomic.AddInt64(&attemptNum, 1)
				tag := fmt.Sprintf("%d/%d", attempt, totalAccounts)

				success, emailAddr, errStr := registerOne(workerID, tag, proxy, outputFile, defaultPassword, defaultDomain, &printMu, &fileMu, logger)
				if success {
					atomic.AddInt64(&successCount, 1)
					ts := time.Now().Format("15:04:05")
					emitLine(&printMu, logger, "[%s] [W%d] SUCCESS: %s\n", ts, workerID, emailAddr)
				} else {
					atomic.AddInt64(&failureCount, 1)
					// Failed — return the slot so it gets retried
					atomic.AddInt64(&remaining, 1)
					ts := time.Now().Format("15:04:05")

					if strings.Contains(errStr, "unsupported_email") {
						parts := strings.Split(emailAddr, "@")
						if len(parts) == 2 {
							domain := parts[1]
							email.AddBlacklistDomain(domain)
							emitLine(&printMu, logger, "[%s] [W%d] Blacklisted domain: %s\n", ts, workerID, domain)
						}
					}

					emitLine(&printMu, logger, "[%s] [W%d] FAILURE: %s | %s\n", ts, workerID, emailAddr, errStr)
				}
			}
		}(w)
	}

	wg.Wait()

	elapsed := time.Since(startTime)
	elapsedStr := formatDuration(elapsed)

	emitLine(&printMu, logger, "\n--- Batch Registration Summary ---\n")
	emitLine(&printMu, logger, "Target:    %d\n", totalAccounts)
	emitLine(&printMu, logger, "Success:   %d\n", successCount)
	emitLine(&printMu, logger, "Attempts:  %d\n", attemptNum)
	emitLine(&printMu, logger, "Failures:  %d\n", failureCount)
	emitLine(&printMu, logger, "Elapsed:   %s\n", elapsedStr)
	emitLine(&printMu, logger, "----------------------------------\n")

	return BatchSummary{
		Target:    totalAccounts,
		Success:   int(successCount),
		Attempts:  int(attemptNum),
		Failures:  int(failureCount),
		Elapsed:   elapsedStr,
		StartedAt: startTime,
		EndedAt:   time.Now(),
	}
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60

	if h > 0 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
