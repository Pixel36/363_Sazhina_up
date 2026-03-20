package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ProbeOutcome struct {
	Target    string
	Reachable bool
	RTT       time.Duration
	Error     error
	Stamp     time.Time
}

func main() {
	var (
		listFile    string
		continuous  bool
		pollPeriod  time.Duration
		outFile     string
		dialTimeout time.Duration
		maxProbes   int
	)

	flag.StringVar(&listFile, "f", "", "путь к файлу со списком хостов")
	flag.BoolVar(&continuous, "monitor", false, "режим непрерывного мониторинга")
	flag.DurationVar(&pollPeriod, "i", 5*time.Second, "интервал между проверками")
	flag.StringVar(&outFile, "o", "", "файл для записи результатов")
	flag.DurationVar(&dialTimeout, "t", 2*time.Second, "таймаут подключения")
	flag.IntVar(&maxProbes, "c", 0, "количество проверок на хост (0 = бесконечно)")

	flag.Parse()

	if listFile == "" {
		fmt.Fprintln(os.Stderr, "ошибка: не указан обязательный флаг -f (файл со списком хостов)")
		flag.Usage()
		os.Exit(1)
	}

	var outputWriter io.Writer
	if outFile != "" {
		fh, err := os.Create(outFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "не удалось создать файл вывода: %v\n", err)
			os.Exit(1)
		}
		defer fh.Close()
		outputWriter = fh
	} else {
		outputWriter = os.Stdout
	}

	targets, err := loadTargets(listFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ошибка чтения файла с хостами: %v\n", err)
		os.Exit(1)
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "файл не содержит ни одного хоста")
		os.Exit(1)
	}

	rootCtx, stopAll := context.WithCancel(context.Background())
	defer stopAll()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Fprintln(os.Stderr, "\nполучен сигнал завершения, останавливаемся...")
		stopAll()
	}()

	resultChan := make(chan ProbeOutcome, 100)

	if continuous {
		var wgProbes sync.WaitGroup
		for _, t := range targets {
			wgProbes.Add(1)
			go monitorProbe(rootCtx, &wgProbes, t, pollPeriod, dialTimeout, maxProbes, resultChan)
		}

		listenerDone := make(chan struct{})
		go func() {
			defer close(listenerDone)
			for res := range resultChan {
				printOutcome(outputWriter, res)
			}
		}()

		allProbesDone := make(chan struct{})
		go func() {
			wgProbes.Wait()
			close(allProbesDone)
		}()

		select {
		case <-rootCtx.Done():
		case <-allProbesDone:
			stopAll()
		}

		wgProbes.Wait()
		close(resultChan)
		<-listenerDone
	} else {
		var wgProbes sync.WaitGroup
		for _, t := range targets {
			wgProbes.Add(1)
			go singleProbe(rootCtx, &wgProbes, t, dialTimeout, resultChan)
		}

		go func() {
			wgProbes.Wait()
			close(resultChan)
		}()

		for res := range resultChan {
			printOutcome(outputWriter, res)
		}
	}
}

func loadTargets(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var list []string
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimRight(line, ",")
		list = append(list, line)
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

func checkHost(ctx context.Context, host string, timeout time.Duration) (bool, time.Duration, error) {
	d := net.Dialer{Timeout: timeout}
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", host+":80")
	if err != nil {
		return false, 0, err
	}
	conn.Close()
	elapsed := time.Since(start)
	return true, elapsed, nil
}

func singleProbe(ctx context.Context, wg *sync.WaitGroup, host string, timeout time.Duration, out chan<- ProbeOutcome) {
	defer wg.Done()
	ok, lat, err := checkHost(ctx, host, timeout)
	res := ProbeOutcome{
		Target:    host,
		Reachable: ok,
		RTT:       lat,
		Error:     err,
		Stamp:     time.Now(),
	}
	select {
	case out <- res:
	case <-ctx.Done():
	}
}

func monitorProbe(ctx context.Context, wg *sync.WaitGroup, host string, interval, timeout time.Duration, maxCount int, out chan<- ProbeOutcome) {
	defer wg.Done()
	count := 0
	for {
		if maxCount > 0 && count >= maxCount {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}

		ok, lat, err := checkHost(ctx, host, timeout)
		res := ProbeOutcome{
			Target:    host,
			Reachable: ok,
			RTT:       lat,
			Error:     err,
			Stamp:     time.Now(),
		}
		select {
		case out <- res:
		case <-ctx.Done():
			return
		}
		count++

		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return
		}
	}
}

func printOutcome(w io.Writer, r ProbeOutcome) {
	ts := r.Stamp.Format("15:04:05")
	if r.Reachable {
		fmt.Fprintf(w, "%s | %s | OK | %v.\n", ts, r.Target, r.RTT)
	} else {
		errMsg := "неизвестная ошибка"
		if r.Error != nil {
			errMsg = r.Error.Error()
		}
		fmt.Fprintf(w, "%s | %s | Timeout | %s\n", ts, r.Target, errMsg)
	}
}