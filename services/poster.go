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
	"time"

	"github.com/anacrolix/ffprobe"
	"github.com/anacrolix/missinggo"
	"github.com/anacrolix/missinggo/v2/resource"
)

// Combines poster instances with automatic storage and single-flight into a given store.
type Poster struct {
	sf    missinggo.SingleFlight
	Store resource.Provider
}

type getPosterOpts func(*PosterInstance)

func CustomGetInfo(custom func(ctx context.Context) (ffprobe.Info, error)) getPosterOpts {
	return func(pi *PosterInstance) {
		pi.customGetInfo = custom
	}
}

func (p *Poster) Get(ctx context.Context, input string, opts ...getPosterOpts) (rc io.ReadCloser, err error) {
	pi := NewPosterInstance(input, opts...)
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

func (me *PosterInstance) defaultGetInfo(ctx context.Context, source string) (info ffprobe.Info, err error) {
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
	info = *pc.Info
	return
}

func (me *PosterInstance) Duration(ctx context.Context) (time.Duration, error) {
	info, err := me.getInfo(ctx, me.input)
	if err != nil {
		return 0, err
	}
	d, err := info.Duration()
	if err != nil {
		return 0, fmt.Errorf("getting duration from info: %w", err)
	}
	return d, nil
}

func (me *PosterInstance) getInfo(ctx context.Context, source string) (ffprobe.Info, error) {
	if me.customGetInfo != nil {
		return me.customGetInfo(ctx)
	}
	return me.defaultGetInfo(ctx, source)
}

func (me *PosterInstance) ssArg(ctx context.Context, source string) (ss string, err error) {
	d, err := me.Duration(ctx)
	if err != nil {
		return "", fmt.Errorf("getting duration from info: %w", err)
	}
	d /= 4
	ss = strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
	return
}

type PosterInstance struct {
	input         string
	customGetInfo func(context.Context) (ffprobe.Info, error)
}

func (me PosterInstance) FFMpegArgs() []string {
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

func (me PosterInstance) HashName() string {
	hash := md5.New()
	hashStrings(hash, me.FFMpegArgs())
	return hex.EncodeToString(hash.Sum(nil)) + ".jpg"
}

// This is exposed so instances can be generated without having to go through Poster, which includes
// storage and single-flight management.
func NewPosterInstance(input string, opts ...getPosterOpts) *PosterInstance {
	ret := PosterInstance{
		input: input,
	}
	for _, opt := range opts {
		opt(&ret)
	}
	return &ret
}

func (me *PosterInstance) WriteTo(ctx context.Context, w io.Writer) (err error) {
	ss, err := me.ssArg(ctx, me.input)
	if err != nil {
		return fmt.Errorf("determining -ss arg value: %w", err)
	}
	return me.WriteToSs(ctx, w, ss)
}

func (me *PosterInstance) WriteToSs(ctx context.Context, w io.Writer, ss string) (err error) {
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
