// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"sync"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog/log"
)

var (
	HTTPDebug = os.Getenv("HTTP_DEBUG") == "true"
	// TODO remove client singleton
	client = http.Client{
		Timeout: 10 * time.Second,
	}
)

func init() {
	if HTTPDebug {
		client.Transport = &loggingTransport{}
	}
}

// DialogMedia is common struct for server and client session and it shares same functionality
// which is mostly arround media
type DialogMedia struct {
	mu sync.Mutex

	// media session is RTP local and remote
	// it is forked on media changes and updated on writer and reader
	// must be mutex protected
	// It MUST be always created on Media Session Init
	// Only safe to use after dialog Answered (Completed state)
	mediaSession *media.MediaSession

	// Packet reader is default reader for RTP audio stream
	// Use always AudioReader to get current Audio reader
	// Use this only as read only
	// It MUST be always created on Media Session Init
	// Only safe to use after dialog Answered (Completed state)
	RTPPacketReader *media.RTPPacketReader

	// Packet writer is default writer for RTP audio stream
	// Use always AudioWriter to get current Audio reader
	// Use this only as read only
	RTPPacketWriter *media.RTPPacketWriter

	// In case we are chaining audio readers
	audioReader io.Reader
	audioWriter io.Writer

	formats sdp.Formats

	onClose func()

	closed bool
}

func (d *DialogMedia) Close() {
	// Any hook attached
	// Prevent double exec
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true

	onClose := d.onClose
	d.onClose = nil
	m := d.mediaSession

	d.mu.Unlock()

	if onClose != nil {
		onClose()
	}

	if m != nil {
		m.Close()
	}
}

func (d *DialogMedia) OnClose(f func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onCloseUnsafe(f)
}

func (d *DialogMedia) onCloseUnsafe(f func()) {
	if d.onClose != nil {
		prev := d.onClose
		d.onClose = func() {
			prev()
			f()
		}
		return
	}
	d.onClose = f
}

func (d *DialogMedia) InitMediaSession(m *media.MediaSession, r *media.RTPPacketReader, w *media.RTPPacketWriter) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initMediaSessionUnsafe(m, r, w)
}

func (d *DialogMedia) initMediaSessionUnsafe(m *media.MediaSession, r *media.RTPPacketReader, w *media.RTPPacketWriter) {
	d.mediaSession = m
	d.RTPPacketReader = r
	d.RTPPacketWriter = w
}

func (d *DialogMedia) createMediaSession(formats sdp.Formats) (*media.MediaSession, error) {
	// TODO we may want to give this control caller or try to figure out based on SIP routing
	ip, _, err := sip.ResolveInterfacesIP("ip4", nil)
	if err != nil {
		return nil, err
	}

	laddr := &net.UDPAddr{IP: ip, Port: 0}
	sess, err := media.NewMediaSession(laddr)
	sess.Formats = formats
	return sess, err
}

// Must be protected with lock
func (d *DialogMedia) sdpReInviteUnsafe(sdp []byte) error {
	msess := d.mediaSession.Fork()
	if err := msess.RemoteSDP(sdp); err != nil {
		log.Error().Err(err).Msg("reinvite media remote SDP applying failed")
		return fmt.Errorf("Malformed SDP")
	}

	d.mediaSession = msess

	rtpSess := media.NewRTPSession(msess)
	d.onCloseUnsafe(func() {
		if err := rtpSess.Close(); err != nil {
			log.Error().Err(err).Msg("Closing session")
		}
	})

	d.RTPPacketReader.UpdateRTPSession(rtpSess)
	d.RTPPacketWriter.UpdateRTPSession(rtpSess)
	rtpSess.MonitorBackground()

	log.Info().
		Str("formats", msess.Formats.String()).
		Str("localAddr", msess.Laddr.String()).
		Str("remoteAddr", msess.Raddr.String()).
		Msg("Media/RTP session updated")
	return nil
}

type AudioReaderOption func(d *DialogMedia) error

type MediaProps struct {
	Codec media.Codec
	Laddr string
	Raddr string
}

func WithAudioReaderMediaProps(p *MediaProps) AudioReaderOption {
	return func(d *DialogMedia) error {
		p.Codec = media.CodecFromSession(d.mediaSession)
		p.Laddr = d.mediaSession.Laddr.String()
		p.Raddr = d.mediaSession.Raddr.String()
		return nil
	}
}

// AudioReader gets current audio reader. It MUST be called after Answer.
// Use AuidioListen for optimized reading.
// Reading buffer should be equal or bigger of media.RTPBufSize
func (d *DialogMedia) AudioReader(opts ...AudioReaderOption) (io.Reader, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, o := range opts {
		if err := o(d); err != nil {
			return nil, err
		}
	}
	return d.getAudioReader(), nil
}

func (d *DialogMedia) getAudioReader() io.Reader {
	if d.audioReader != nil {
		return d.audioReader
	}
	return d.RTPPacketReader
}

// audioReaderProps
func (d *DialogMedia) audioReaderProps(p *MediaProps) io.Reader {
	d.mu.Lock()
	defer d.mu.Unlock()

	WithAudioReaderMediaProps(p)(d)
	return d.getAudioReader()
}

// SetAudioReader adds/changes audio reader.
// Use this when you want to have interceptors of your audio
func (d *DialogMedia) SetAudioReader(r io.Reader) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.audioReader = r
}

type AudioWriterOption func(d *DialogMedia) error

func WithAudioWriterMediaProps(p *MediaProps) AudioWriterOption {
	return func(d *DialogMedia) error {
		p.Codec = media.CodecFromSession(d.mediaSession)
		p.Laddr = d.mediaSession.Laddr.String()
		p.Raddr = d.mediaSession.Raddr.String()
		return nil
	}
}

func (d *DialogMedia) AudioWriter(opts ...AudioWriterOption) (io.Writer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, o := range opts {
		if err := o(d); err != nil {
			return nil, err
		}
	}

	return d.getAudioWriter(), nil
}

func (d *DialogMedia) getAudioWriter() io.Writer {
	if d.audioWriter != nil {
		return d.audioWriter
	}
	return d.RTPPacketWriter
}

func (d *DialogMedia) audioWriterProps(p *MediaProps) io.Writer {
	d.mu.Lock()
	defer d.mu.Unlock()

	WithAudioWriterMediaProps(p)(d)
	return d.getAudioWriter()
}

// SetAudioWriter adds/changes audio reader.
// Use this when you want to have interceptors of your audio
func (d *DialogMedia) SetAudioWriter(r io.Writer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.audioWriter = r
}

func (d *DialogMedia) Media() *DialogMedia {
	return d
}

// PlaybackCreate creates playback for audio
func (d *DialogMedia) PlaybackCreate() (AudioPlayback, error) {
	mprops := MediaProps{}
	w := d.audioWriterProps(&mprops)
	if w == nil {
		return AudioPlayback{}, fmt.Errorf("no media setup")
	}
	p := NewAudioPlayback(w, mprops.Codec)
	return p, nil
}

// PlaybackControlCreate creates playback for audio with controls like mute unmute
func (d *DialogMedia) PlaybackControlCreate() (AudioPlaybackControl, error) {
	// NOTE we should avoid returning pointers for any IN dialplan to avoid heap
	mprops := MediaProps{}
	w := d.audioWriterProps(&mprops)

	if w == nil {
		return AudioPlaybackControl{}, fmt.Errorf("no media setup")
	}
	// Audio is controled via audio reader/writer
	control := &audioControl{
		Writer: w,
	}

	p := AudioPlaybackControl{
		AudioPlayback: NewAudioPlayback(control, mprops.Codec),
		control:       control,
	}
	return p, nil
}

type loggingTransport struct{}

func (s *loggingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	bytes, _ := httputil.DumpRequestOut(r, false)

	resp, err := http.DefaultTransport.RoundTrip(r)
	// err is returned after dumping the response

	respBytes, _ := httputil.DumpResponse(resp, false)
	bytes = append(bytes, respBytes...)

	log.Debug().Msgf("HTTP Debug:\n%s\n", bytes)

	return resp, err
}

func (d *DialogMedia) Listen() error {
	buf := make([]byte, media.RTPBufSize)
	audioRader := d.getAudioReader()
	for {
		_, err := audioRader.Read(buf)
		if err != nil {
			return err
		}
	}
}

func (d *DialogMedia) ListenContext(ctx context.Context) error {
	buf := make([]byte, media.RTPBufSize)
	go func() {
		<-ctx.Done()
		d.mediaSession.StopRTP(2, 0)
	}()
	audioRader := d.getAudioReader()
	for {
		_, err := audioRader.Read(buf)
		if err != nil {
			return err
		}
	}
}

func (d *DialogMedia) ListenUntil(dur time.Duration) error {
	buf := make([]byte, media.RTPBufSize)

	d.mediaSession.StopRTP(2, dur)
	audioReader := d.getAudioReader()
	for {
		_, err := audioReader.Read(buf)
		if err != nil {
			return err
		}
	}
}

type DTMFReader struct {
	mediaSession *media.MediaSession
	dtmfReader   *media.RTPDtmfReader
}

// AudioReaderDTMF is DTMF over RTP. It reads audio and provides hook for dtmf while listening for audio
func (m *DialogMedia) AudioReaderDTMF() *DTMFReader {
	return &DTMFReader{
		dtmfReader:   media.NewRTPDTMFReader(media.CodecTelephoneEvent8000, m.RTPPacketReader, m.getAudioReader()),
		mediaSession: m.mediaSession,
	}
}

func (d *DTMFReader) Listen(onDTMF func(dtmf rune) error, dur time.Duration) error {
	buf := make([]byte, media.RTPBufSize)
	for {
		if _, _, err := d.AudioRead(buf, onDTMF, dur); err != nil {
			return err
		}
	}
}

func (d *DTMFReader) AudioRead(buf []byte, onDTMF func(dtmf rune) error, dur time.Duration) (n int, t uint8, err error) {
	mediaSession := d.mediaSession
	if dur > 0 {
		// Stop RTP
		mediaSession.StopRTP(1, dur)
		defer mediaSession.StartRTP(2)
	}
	// This is optimal way of reading audio and DTMF
	dtmfReader := d.dtmfReader
	n, t, err = dtmfReader.Read(buf)
	if err != nil {
		return n, t, err
	}

	if dtmf, ok := dtmfReader.ReadDTMF(); ok {
		if err := onDTMF(dtmf); err != nil {
			return n, t, err
		}
	}
	return n, t, nil
}

type DTMFWriter struct {
	mediaSession *media.MediaSession
	dtmfReader   *media.RTPDtmfWriter
}

func (m *DialogMedia) AudioWriterDTMF() *DTMFWriter {
	return &DTMFWriter{
		dtmfReader:   media.NewRTPDTMFWriter(media.CodecTelephoneEvent8000, m.RTPPacketWriter, m.getAudioWriter()),
		mediaSession: m.mediaSession,
	}
}

func (w *DTMFWriter) WriteDTMF(dtmf rune) error {
	return w.dtmfReader.WriteDTMF(dtmf)
}
