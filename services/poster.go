package services

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"strconv"

	"github.com/anacrolix/ffprobe"
	"github.com/anacrolix/missinggo"
	"github.com/anacrolix/missinggo/v2/resource"
)

type Poster struct {
	sf    missinggo.SingleFlight
	Store resource.Provider
}

func (p *Poster) Get(ctx context.Context, input string) (rc io.ReadCloser, err error) {
	pi := NewPosterInstance(input)
	p.sf.Lock(pi.HashName())
	defer p.sf.Unlock(pi.HashName())
	stored, err := p.Store.NewInstance(pi.HashName())
	if err != nil {
		return
	}
	stat, err := stored.Stat()
	if err == nil && stat.Size() != 0 {
		rc, err = stored.Get()
		if err == nil {
			return
		}
	}
	r, w := io.Pipe()
	go func() {
		w.CloseWithError(pi.WriteTo(ctx, w))
	}()
	err = stored.Put(r)
	if err != nil {
		stored.Delete()
		return
	}
	return stored.Get()
}

func hashStrings(h hash.Hash, ss []string) {
	for _, s := range ss {
		h.Write([]byte(s))
	}
}

func posterSSArg(ctx context.Context, source string) (ss string, err error) {
	pc, err := ffprobe.Start(source)
	if err != nil {
		return
	}
	select {
	case <-ctx.Done():
		pc.Cmd.Process.Kill()
		err = ctx.Err()
		return
	case <-pc.Done:
	}
	err = pc.Err
	if err != nil {
		return
	}
	d, err := pc.Info.Duration()
	if err != nil {
		return "", fmt.Errorf("getting duration from info: %w", err)
	}
	d /= 4
	ss = strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
	return
}

type posterInstance struct {
	input string
}

func (me posterInstance) FFMpegArgs() []string {
	return []string{
		// Doesn't work with rmvb.
		// "-skip_frame", "nokey",
		"-i", me.input,
		"-vf", "thumbnail",
		"-frames:v", "1",
		"-f", "image2pipe",
		"pipe:",
	}
}

func (me posterInstance) HashName() string {
	hash := md5.New()
	hashStrings(hash, me.FFMpegArgs())
	return hex.EncodeToString(hash.Sum(nil)) + ".jpg"
}

func NewPosterInstance(input string) (ret *posterInstance) {
	return &posterInstance{
		input: input,
	}
}

func (me *posterInstance) WriteTo(ctx context.Context, w io.Writer) (err error) {
	ss, err := posterSSArg(ctx, me.input)
	if err != nil {
		return fmt.Errorf("determining -ss arg value: %w", err)
	}
	args := []string{
		"-xerror",
		"-loglevel", "warning",
		"-ss", ss,
	}
	args = append(args, me.FFMpegArgs()...)
	cmd := exec.Command("ffmpeg", args...)
	cmd.Stderr = os.Stderr
	cmd.Stdout = w
	err = cmd.Start()
	if err != nil {
		return
	}
	go func() {
		<-ctx.Done()
		cmd.Process.Kill()
	}()
	err = cmd.Wait()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return
}
