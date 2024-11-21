// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"io"

	"github.com/rs/zerolog/log"
)

type RTPDtmfReader struct {
	codec        Codec // Depends on media session. Defaults to 101 per current mapping
	reader       io.Reader
	packetReader *RTPPacketReader

	lastEv  DTMFEvent
	dtmf    rune
	dtmfSet bool
}

// RTP DTMF writer is midleware for reading DTMF events
// It reads from io Reader and checks packet Reader
func NewRTPDTMFReader(codec Codec, packetReader *RTPPacketReader, reader io.Reader) *RTPDtmfReader {
	return &RTPDtmfReader{
		codec:        codec,
		packetReader: packetReader,
		reader:       reader,
		// dmtfs:        make([]rune, 0, 5), // have some
	}
}

// Write is RTP io.Writer which adds more sync mechanism
func (w *RTPDtmfReader) Read(b []byte) (int, uint8, error) {
	n, err := w.reader.Read(b)
	if err != nil {
		// Signal our reader that no more dtmfs will be read
		// close(w.dtmfCh)
		return n, 0, err
	}

	// Check is this DTMF
	hdr := w.packetReader.PacketHeader
	if hdr.PayloadType != w.codec.PayloadType {
		return n, hdr.PayloadType, nil
	}

	// Now decode DTMF
	ev := DTMFEvent{}
	if err := DTMFDecode(b, &ev); err != nil {
		log.Error().Err(err).Msg("Failed to decode DTMF event")
	}
	w.processDTMFEvent(ev)
	return n, hdr.PayloadType, nil
}

func (w *RTPDtmfReader) processDTMFEvent(ev DTMFEvent) {
	log.Debug().Interface("ev", ev).Msg("Processing DTMF event")
	if ev.EndOfEvent {
		// Does this match to our last ev
		if w.lastEv.Event != ev.Event {
			return
		}

		dur := ev.Duration - w.lastEv.Duration
		if dur <= 3*160 { // Expect at least ~50ms duration
			log.Debug().Uint16("dur", dur).Msg("Received DTMF packet but short duration")
			return
		}

		w.dtmf = DTMFToRune(ev.Event)
		w.dtmfSet = true
		// select {
		// case w.dtmfCh <- byte(ev.Event):
		// default:
		// 	log.Warn().Msg("DTMF event missed")
		// }
		// Reset last ev
		w.lastEv = DTMFEvent{}
		return
	}
	if w.lastEv.Event == ev.Event {
		return
	}
	w.lastEv = ev
}

func (w *RTPDtmfReader) ReadDTMF() (rune, bool) {
	defer func() { w.dtmfSet = false }()
	return w.dtmf, w.dtmfSet
	// dtmf, ok := <-w.dtmfCh
	// return DTMFToRune(dtmf), ok
}
