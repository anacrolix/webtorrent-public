package transcoder

import (
	"context"
	"errors"
	"fmt"
	"github.com/anacrolix/missinggo/v2/panicif"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/anacrolix/ffprobe"
	"github.com/anacrolix/log"
	"github.com/anacrolix/missinggo/perf"
	"github.com/anacrolix/sync"
)

func probeDuration(input string) (d time.Duration, err error) {
	defer perf.ScopeTimer()()
	info, err := ffprobe.Run(input)
	if err != nil {
		err = fmt.Errorf("error probing: %s", err)
		return
	}
	return info.Duration()
}

func probeDurationSettingProgress(input string, set func(func(*Progress))) {
	set(func(p *Progress) {
		p.Probing = true
	})
	dur, err := probeDuration(input)
	if err != nil {
		log.Printf("error probing duration: %s", err)
	}
	set(func(p *Progress) {
		if err == nil {
			p.InputDuration = dur
		}
		p.Probing = false
	})
}

type progressWriter struct {
	progress, total int64
	callback        func(fraction float64)
}

func (me *progressWriter) Write(b []byte) (int, error) {
	me.progress += int64(len(b))
	me.callback(float64(me.progress) / float64(me.total))
	return len(b), nil
}

func downloadInput(ctx context.Context, url, to string, progress func(progress float64)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	panicif.Err(err)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("got status code %d", resp.StatusCode)
	}
	errChan := make(chan error, 1)
	go func() {
		errChan <- func() error {
			f, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
			if err != nil {
				return err
			}
			go func() {
				<-ctx.Done()
				f.Close()
			}()
			_, err = io.Copy(f, io.TeeReader(resp.Body, &progressWriter{
				total:    resp.ContentLength,
				callback: progress,
			}))
			if err != nil {
				return err
			}
			return f.Close()
		}()
	}()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case err := <-errChan:
		// Should we return the context error here instead if it exists (cmp.Or) or let the caller
		// figure that out?
		return err
	}
}

func updateDownloadProgress(f func(func(*Progress))) func(float64) {
	return func(fraction float64) {
		f(func(p *Progress) {
			p.DownloadProgress = fraction
		})
	}
}

func transcode(
	ctx context.Context,
	url, tempFilePath, logPath, outputName string,
	args []string,
	updateProgress func(func(*Progress)),
) error {
	updateProgress(func(p *Progress) {
		p.Downloading = true
	})
	defer os.Remove(tempFilePath)
	if err := downloadInput(ctx, url, tempFilePath, updateDownloadProgress(updateProgress)); err != nil {
		return fmt.Errorf("error downloading %q: %w", url, err)
	}
	updateProgress(func(p *Progress) {
		p.Downloading = false
	})

	go probeDurationSettingProgress(tempFilePath, updateProgress)

	os.MkdirAll(filepath.Dir(logPath), 0750)
	// Log files are left behind by failed runs, so don't try again if it
	// already exists. TODO: Open a temporary log path, then move over to the path that should be
	// checked for previous failed runs after running the transcode to completion (failure or
	// success).
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	if errors.Is(cmd.Err, exec.ErrDot) {
		cmd.Err = nil
	}
	cmd.Stderr = logFile
	log.Printf("invoking %q", args)
	started := time.Now()
	defer func() {
		log.Printf("%v ran for %v", outputName, time.Since(started))
		updateProgress(func(p *Progress) {
			p.Converting = false
		})
	}()
	updateProgress(func(p *Progress) {
		p.Converting = true
	})
	return cmd.Run()
}

type operation struct {
	mu        sync.Mutex
	Progress  Progress
	sendEvent func()
}

func (op *operation) updateProgress(f func(p *Progress)) {
	op.mu.Lock()
	defer op.mu.Unlock()
	before := op.Progress
	f(&op.Progress)
	if op.Progress != before {
		// log.Printf("%#v", op.Progress)
		op.sendEvent()
	}
}
