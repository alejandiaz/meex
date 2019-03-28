package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"time"
	"unicode"

	"github.com/busoc/meex"
	"github.com/busoc/meex/cmd/internal/multireader"
	"github.com/busoc/timutil"
	"github.com/midbel/xxh"
)

var ErrInvalid = errors.New("invalid")

type shortError struct {
	Want, Got int
}

func (e shortError) Error() string {
	if e.Want == 0 {
		return fmt.Sprintf("short buffer: not enough bytes available to read headers (%d)", e.Got)
	}
	return fmt.Sprintf("short buffer: got %d bytes, want %d bytes", e.Got, e.Want)
}

func isShortError(err error) bool {
	_, ok := err.(shortError)
	return ok
}

func NotEnoughByte(want, got int) error {
	return shortError{want, got}
}

const (
	UPILen        = 32
	HRDLHeaderLen = 18
	VMUHeaderLen  = 24
)

var (
	modeRT = []byte("realtime")
	modePB = []byte("playback")

	chanVic1 = []byte("vic1")
	chanVic2 = []byte("vic2")
	chanLRSD = []byte("lrsd")

	unknown = []byte("***")
	invalid = []byte("invalid")
)

const listRow = "%8d | %04x || %s | %9d | %s | %s || %02x | %s | %7d | %16s | %08x | %8s || %08x\n"

func main() {
	mem := flag.String("m", "", "memory profile")
	cpu := flag.String("c", "", "cpu profile")
	gc := flag.Int("g", 0, "gc percent")
	withError := flag.Bool("e", false, "include invalid packets")
	flag.Parse()

	if *gc > 0 {
		debug.SetGCPercent(*gc)
	}
	if *cpu != "" {
		w, err := os.Create(*cpu)
		if err != nil {
			os.Exit(77)
		}
		defer w.Close()
		if err := pprof.StartCPUProfile(w); err != nil {
			os.Exit(78)
		}
		defer pprof.StopCPUProfile()
	}

	if *mem != "" {
		defer func() {
			w, err := os.Create(*mem)
			if err != nil {
				return
			}
			defer w.Close()
			runtime.GC()
			if err := pprof.WriteHeapProfile(w); err != nil {
				return
			}
		}()
	}

	mr, err := multireader.New(flag.Args(), true)
	if err != nil {
		return
	}
	defer mr.Close()

	digest := xxh.New64(0)
	rt := io.TeeReader(meex.NewReader(mr), digest)

	buffer := make([]byte, meex.MaxBufferSize)

	var total, size, invalid int64
	for {
		n, err := rt.Read(buffer)
		if err != nil {
			if err == io.EOF {
				break
			}
			fmt.Fprintln(os.Stderr, "unexpected error reading rt:", err)
			os.Exit(2)
		}
		z, err := dumpPacket(buffer[:n], digest.Sum64(), *withError)
		total++
		size += int64(z)
		switch {
		case err == nil:
		case isShortError(err) || err == ErrInvalid:
			invalid++
		default:
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	fmt.Fprintln(os.Stdout)
	fmt.Fprintf(os.Stdout, "%d packets (%dMB, %d invalid)\n", total, size>>20, invalid)
}

func dumpPacket(body []byte, digest uint64, withErr bool) (int, error) {
	if len(body) < HRDLHeaderLen+VMUHeaderLen {
		return 0, NotEnoughByte(0, len(body))
	}
	var (
		h   HRDLHeader
		v   VMUHeader
		c   VMUCommonHeader
		err error
	)
	if h, err = decodeHRDL(body[:HRDLHeaderLen]); err != nil {
		return 0, err
	}
	if v, err = decodeVMU(body[HRDLHeaderLen : HRDLHeaderLen+VMUHeaderLen]); err != nil {
		return 0, err
	}
	if size := len(body) - HRDLHeaderLen - 12; int(v.Size) > size {
		return 0, NotEnoughByte(int(v.Size)+12, size+12)
	}
	if c, err = decodeCommon(body[HRDLHeaderLen+VMUHeaderLen:]); err != nil {
		return 0, err
	}

	sum, bad := calculateSum(body[HRDLHeaderLen+8 : HRDLHeaderLen+8+int(v.Size)+4])
	if bytes.Equal(bad, invalid) && !withErr {
		return 0, ErrInvalid
	}

	vmutime := timeFormat(v.Timestamp(), vmuTimeBuffer)
	acqtime := timeFormat(c.Acquisition(), acqTimeBuffer)
	channel, mode := whichChannel(v.Channel), whichMode(v.Origin, c.Origin)

	// writer, pattern, uint32, uint16, []byte; uint32, []byte, []byte, uint8, []byte, uint32, []byte, uint32, []byte, uint64
	fmt.Fprintf(os.Stdout, listRow, v.Size, h.Error, vmutime, v.Sequence, mode, channel, c.Origin, acqtime, c.Counter, userInfo(c.UPI), sum, bad, digest)
	if bytes.Equal(bad, invalid) {
		err = ErrInvalid
	}
	return int(v.Size), err
}

var (
	upiBuffer     = make([]byte, UPILen)
	vmuTimeBuffer = make([]byte, 0, UPILen)
	acqTimeBuffer = make([]byte, 0, UPILen)
)

const millis = 1000 * 1000

func timeFormat(t time.Time, buf []byte) []byte {
	y, m, d := t.Date()
	buf = strconv.AppendInt(buf, int64(y), 10)
	buf = append(buf, '-')
	if m < 10 {
		buf = strconv.AppendInt(buf, 0, 10)
	}
	buf = strconv.AppendInt(buf, int64(m), 10)
	buf = append(buf, '-')
	if d < 10 {
		buf = strconv.AppendInt(buf, 0, 10)
	}
	buf = strconv.AppendInt(buf, int64(d), 10)
	buf = append(buf, space)
	if t.Hour() < 10 {
		buf = strconv.AppendInt(buf, 0, 10)
	}
	buf = strconv.AppendInt(buf, int64(t.Hour()), 10)
	buf = append(buf, ':')
	if t.Minute() < 10 {
		buf = strconv.AppendInt(buf, 0, 10)
	}
	buf = strconv.AppendInt(buf, int64(t.Minute()), 10)
	buf = append(buf, ':')
	if t.Second() < 10 {
		buf = strconv.AppendInt(buf, 0, 10)
	}
	buf = strconv.AppendInt(buf, int64(t.Second()), 10)

	buf = append(buf, '.')
	ms := t.Nanosecond() / millis
	if ms < 10 {
		buf = strconv.AppendInt(buf, 0, 10)
	}
	if ms < 100 {
		buf = strconv.AppendInt(buf, 0, 10)
	}
	buf = strconv.AppendInt(buf, int64(ms), 10)

	return buf
}

func userInfo(upi [UPILen]byte) []byte {
	var n int
	for i := 0; i < UPILen; i++ {
		keep, done := shouldKeepRune(rune(upi[i]))
		if done {
			break
		}
		if !keep {
			continue
		}
		upiBuffer[n] = upi[i]
		n++
	}
	return upiBuffer[:n]
}

type HRDLHeader struct {
	Size         uint32
	Error        uint16
	Channel      uint8
	Payload      uint8
	PacketCoarse uint32
	PacketFine   uint8
	HRDPCoarse   uint32
	HRDPFine     uint8
}

func (h HRDLHeader) Elapsed() time.Duration {
	return h.Archive().Sub(h.Acquisition())
}

func (h HRDLHeader) Acquisition() time.Time {
	return timutil.Join5(h.PacketCoarse, h.PacketFine)
}

func (h HRDLHeader) Archive() time.Time {
	return timutil.Join5(h.HRDPCoarse, h.HRDPFine)
}

type VMUHeader struct {
	Size     uint32
	Channel  uint8
	Origin   uint8
	Sequence uint32
	Coarse   uint32
	Fine     uint16
}

func (v VMUHeader) Timestamp() time.Time {
	return timutil.Join6(v.Coarse, v.Fine)
}

type VMUCommonHeader struct {
	Property uint8
	Origin   uint8
	AcqTime  time.Duration
	AuxTime  time.Duration
	Stream   uint16
	Counter  uint32
	UPI      [UPILen]byte
}

func (v VMUCommonHeader) Acquisition() time.Time {
	return timutil.GPS.Add(v.AcqTime)
}

func (v VMUCommonHeader) Auxiliary() time.Time {
	return timutil.GPS.Add(v.AuxTime)
}

func decodeHRDL(body []byte) (HRDLHeader, error) {
	var h HRDLHeader

	h.Size = binary.LittleEndian.Uint32(body)
	h.Error = binary.BigEndian.Uint16(body[4:])
	h.Payload = uint8(body[6])
	h.Channel = uint8(body[7])
	h.PacketCoarse = binary.BigEndian.Uint32(body[8:])
	h.PacketFine = uint8(body[12])
	h.HRDPCoarse = binary.BigEndian.Uint32(body[13:])
	h.HRDPFine = uint8(body[17])

	return h, nil
}

func decodeVMU(body []byte) (VMUHeader, error) {
	var v VMUHeader

	v.Size = binary.LittleEndian.Uint32(body[4:])
	v.Channel = uint8(body[8])
	v.Origin = uint8(body[9])
	v.Sequence = binary.LittleEndian.Uint32(body[12:])
	v.Coarse = binary.LittleEndian.Uint32(body[16:])
	v.Fine = binary.LittleEndian.Uint16(body[20:])

	return v, nil
}

func decodeCommon(body []byte) (VMUCommonHeader, error) {
	var v VMUCommonHeader

	v.Property = body[0]
	v.Stream = binary.LittleEndian.Uint16(body[1:])
	v.Counter = binary.LittleEndian.Uint32(body[3:])
	v.AcqTime = time.Duration(binary.LittleEndian.Uint64(body[7:]))
	v.AuxTime = time.Duration(binary.LittleEndian.Uint64(body[15:]))
	v.Origin = body[23]

	switch v.Property >> 4 {
	case 1: // science
		copy(v.UPI[:], body[24:])
	case 2: // image
		copy(v.UPI[:], body[44:])
	}
	return v, nil
}

func whichChannel(c uint8) []byte {
	switch c {
	case 1:
		return chanVic1
	case 2:
		return chanVic2
	case 3:
		return chanLRSD
	default:
		return unknown
	}
}

func whichMode(vmu, hrd uint8) []byte {
	if vmu == hrd {
		return modeRT
	}
	return modePB
}

func shouldKeepRune(r rune) (bool, bool) {
	if r == 0 {
		return false, true
	}
	if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
		return true, false
	}
	return false, true
}

func keepRune(r rune) rune {
	if r == 0 {
		return -1
	}
	if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
		return r
	}
	return '*'
}

const blockSize = 16

func calculateSum(body []byte) (uint32, []byte) {
	var (
		sum uint32
		i   int
	)
	limit := len(body) - 4
	for i < (limit-blockSize)+1 {
		sum += uint32(body[i]) + uint32(body[i+1]) + uint32(body[i+2]) + uint32(body[i+3])
		sum += uint32(body[i+4]) + uint32(body[i+5]) + uint32(body[i+6]) + uint32(body[i+7])
		sum += uint32(body[i+8]) + uint32(body[i+9]) + uint32(body[i+10]) + uint32(body[i+11])
		sum += uint32(body[i+12]) + uint32(body[i+13]) + uint32(body[i+14]) + uint32(body[i+15])

		i += blockSize
	}
	for i < limit {
		sum += uint32(body[i])
		i++
	}
	expected := binary.LittleEndian.Uint32(body[limit:])
	var bad []byte
	if expected != sum {
		bad = invalid
	} else {
		bad = unknown
	}
	return sum, bad
}