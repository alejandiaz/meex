package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/busoc/meex"
	"github.com/busoc/vmu"
	"github.com/midbel/cli"
	"github.com/midbel/linewriter"
	"github.com/midbel/xxh"
	"github.com/pkg/profile"
)

const TimeFormat = "2006-01-02 15:04:05.000"

var (
	modeRT = []byte("realtime")
	modePB = []byte("playback")

	chanVic1 = []byte("vic1")
	chanVic2 = []byte("vic2")
	chanLRSD = []byte("lrsd")

	invalid = []byte("invalid")
	unknown = []byte("***")
)

var commands = []*cli.Command{
	{
		Usage: "list [-e] [-i] [-g] <file...>",
		Short: "",
		Run:   runList,
	},
	{
		Usage: "diff [-e] [-i] [-g] <file...>",
		Short: "",
		Run:   runDiff,
	},
	{
		Usage: "count [-e] [-b] [-g] <file...>",
		Short: "",
		Run:   runCount,
	},
}

const helpText = `{{.Name}} scan the HRDP archive to consolidate the USOC HRDP archive

Usage:

  {{.Name}} command [options] <arguments>

Available commands:

{{range .Commands}}{{if .Runnable}}{{printf "  %-12s %s" .String .Short}}{{if .Alias}} (alias: {{ join .Alias ", "}}){{end}}{{end}}
{{end}}
Use {{.Name}} [command] -h for more information about its usage.
`

func main() {
	// defer profile.Start(profile.CPUProfile).Stop()
	defer profile.Start(profile.MemProfile).Stop()
	defer func() {
		if err := recover(); err != nil {
			log.Fatalf("unexpected error: %s", err)
		}
	}()
	log.SetFlags(0)
	if err := cli.Run(commands, cli.Usage("vrx", helpText, commands), nil); err != nil {
		log.Fatalln(err)
	}
}

func runList(cmd *cli.Command, args []string) error {
	keepInvalid := cmd.Flag.Bool("e", false, "keep invalid packets")
	if err := cmd.Flag.Parse(args); err != nil {
		return err
	}
	d, err := Decode(cmd.Flag.Args())
	if err != nil {
		return err
	}
	line := linewriter.New(1024, 1)
	seen := make(map[uint8]vmu.Packet)

	var invalid, size, missing, skipped int
	for i := 0; ; i++ {
		p, err := d.Decode(false)
		switch err {
		case nil, vmu.ErrInvalid:
			if err == vmu.ErrInvalid {
				invalid++
				if !*keepInvalid {
					continue
				}
			}
			var diff uint32
			if prev, ok := seen[p.VMUHeader.Channel]; ok {
				diff = p.Missing(prev)
				missing += int(diff)
			}
			seen[p.VMUHeader.Channel] = p

			dumpPacket(line, p, diff, err != vmu.ErrInvalid)
			size += int(p.VMUHeader.Size)
		case vmu.ErrSkip:
      skipped++
      i--
		case io.EOF:
			log.Printf("%d packets (%dMB, %d invalid, %d missing, %d skipped)\n", i, size>>20, invalid, missing, skipped)
			return nil
		default:
			return err
		}
	}
}
func runCount(cmd *cli.Command, args []string) error {
	keepInvalid := cmd.Flag.Bool("e", false, "keep invalid packets")
	by := cmd.Flag.String("b", "", "count packets by channel or origin")
	if err :=  cmd.Flag.Parse(args); err != nil {
		return err
	}
	var (
		getBy func(vmu.Packet) uint8
		missBy func(vmu.Packet, vmu.Packet) uint64
		appendBy func(*linewriter.Writer, uint8)
	)
	switch strings.ToLower(*by) {
	case "channel", "":
		getBy = byChannel
		missBy = func(p, prev vmu.Packet) uint64 {
			if p.VMUHeader.Sequence < prev.VMUHeader.Sequence {
				return 0
			}
			return uint64(p.VMUHeader.Sequence - prev.VMUHeader.Sequence) - 1
		}
		appendBy = func(line *linewriter.Writer, c uint8) {
			line.AppendBytes(whichChannel(c), 4, linewriter.Text | linewriter.AlignLeft)
		}
	case "origin":
		getBy = byOrigin
		missBy = func(p, prev vmu.Packet) uint64 {
			if p.DataHeader.Counter < prev.DataHeader.Counter {
				return 0
			}
			return uint64(p.DataHeader.Counter - prev.DataHeader.Counter) - 1
		}
		appendBy = func(line *linewriter.Writer, c uint8) {
			line.AppendUint(uint64(c), 2, linewriter.AlignCenter | linewriter.Hex | linewriter.WithZero)
		}
	default:
		return fmt.Errorf("unknown value %s", *by)
	}

	d, err := Decode(cmd.Flag.Args())
	if err != nil {
		return err
	}
	stats := make(map[uint8]meex.Coze)
	seen := make(map[uint8]vmu.Packet)
	for {
		p, err := d.Decode(false)
		switch err {
		case nil, vmu.ErrInvalid:
			by := getBy(p)
			cz := stats[by]

			cz.Count++
			cz.Size += uint64(p.VMUHeader.Size)
			if err == vmu.ErrInvalid {
				cz.Error++
				if !*keepInvalid {
					continue
				}
			}
			if prev, ok := seen[by]; ok {
				cz.Missing += missBy(p, prev)
			}
			seen[by], stats[by] = p, cz
		case vmu.ErrSkip:
		case io.EOF:
			line := linewriter.NewWriter(1024, 1, ' ')
			for b, cz := range stats {
				appendBy(line, b)
				line.AppendUint(cz.Count, 6, linewriter.AlignRight)
				line.AppendUint(cz.Missing, 6, linewriter.AlignRight)
				line.AppendUint(cz.Error, 6, linewriter.AlignRight)
				line.AppendUint(cz.Size, 6, linewriter.AlignRight)

				os.Stdout.Write(append(line.Bytes(), '\n'))
				line.Reset()
			}
			return nil
		default:
			return err
		}
	}
}

func runDiff(cmd *cli.Command, args []string) error {
	keepInvalid := cmd.Flag.Bool("e", false, "keep invalid packets")
	by := cmd.Flag.String("b", "", "count packets by channel or origin")
	duration := cmd.Flag.Duration("d", time.Second, "maximum gap duration")
	if err := cmd.Flag.Parse(args); err != nil {
		return err
	}

	var (
		getBy func(vmu.Packet) uint8
		gapBy func(vmu.Packet, vmu.Packet, time.Duration) (bool, meex.Gap)
		appendBy func(*linewriter.Writer, vmu.Packet)
	)
	switch strings.ToLower(*by) {
	case "channel", "":
		getBy = byChannel
		gapBy = gapByChannel
		appendBy = func(line *linewriter.Writer, p vmu.Packet) {
			line.AppendBytes(whichChannel(p.VMUHeader.Channel), 4, linewriter.Text | linewriter.AlignLeft)
		}
	case "origin":
		getBy = byOrigin
		gapBy = gapByOrigin
		appendBy = func(line *linewriter.Writer, p vmu.Packet) {
			line.AppendBytes(whichChannel(p.VMUHeader.Channel), 4, linewriter.Text | linewriter.AlignLeft)
			line.AppendUint(uint64(p.DataHeader.Origin), 2, linewriter.AlignCenter | linewriter.Hex | linewriter.WithZero)
		}
	default:
		return fmt.Errorf("unknown value %s", *by)
	}

	d, err := Decode(cmd.Flag.Args())
	if err != nil {
		return err
	}
	seen := make(map[uint8]vmu.Packet)
	line := linewriter.NewWriter(1024, 1, ' ')
	for {
		p, err := d.Decode(false)
		switch err {
		case nil, vmu.ErrInvalid:
			if err == vmu.ErrInvalid && !*keepInvalid {
				continue
			}
			by := getBy(p)
			if prev, ok := seen[by]; ok {
				if ok, g := gapBy(p, prev, *duration); ok {
					appendBy(line, p)
					line.AppendTime(g.Starts, "2006-01-02 15:04:05.000", linewriter.AlignRight)
					line.AppendTime(g.Ends, "2006-01-02 15:04:05.000", linewriter.AlignRight)
					line.AppendInt(int64(g.Last), 8, linewriter.AlignRight)
					line.AppendInt(int64(g.First), 8, linewriter.AlignRight)
					line.AppendInt(int64(g.Missing()), 8, linewriter.AlignRight)
					line.AppendString(g.Duration().String(), 10, linewriter.AlignRight)

					os.Stdout.Write(append(line.Bytes(), '\n'))
					line.Reset()
				}
			}
			seen[by] = p
		case vmu.ErrSkip:
		case io.EOF:
			return nil
		default:
			return err
		}
	}
	return nil
}

func Decode(files []string) (*vmu.Decoder, error) {
	mr, err := meex.Browse(files, true)
	if err != nil {
		return nil, err
	}
	return vmu.NewDecoder(meex.NewReader(mr)), nil
}

func dumpPacket(line *linewriter.Writer, p vmu.Packet, missing uint32, valid bool) {
	defer line.Reset()

	h, v, c := p.HRDPHeader, p.VMUHeader, p.DataHeader

	var bad []byte
	if !valid {
		bad = invalid
	} else {
		bad = unknown
	}

	line.AppendUint(uint64(v.Size), 7, linewriter.AlignRight)
	line.AppendUint(uint64(h.Error), 4, linewriter.AlignRight|linewriter.Hex|linewriter.WithZero)
	// packet VMU info
	line.AppendTime(v.Timestamp(), TimeFormat, linewriter.AlignCenter)
	line.AppendUint(uint64(v.Sequence), 7, linewriter.AlignRight)
	line.AppendUint(uint64(missing), 3, linewriter.AlignRight)
	line.AppendBytes(whichMode(p.IsRealtime()), 8, linewriter.AlignCenter|linewriter.Text)
	line.AppendBytes(whichChannel(v.Channel), 4, linewriter.AlignCenter|linewriter.Text)
	// packet HRD info
	line.AppendUint(uint64(c.Origin), 2, linewriter.AlignRight|linewriter.Hex|linewriter.WithZero)
	line.AppendTime(c.Acquisition(), TimeFormat, linewriter.AlignCenter)
	line.AppendUint(uint64(c.Counter), 8, linewriter.AlignRight)
	line.AppendBytes(c.UserInfo(), 16, linewriter.AlignLeft|linewriter.Text)
	// packet sums and validity state
	line.AppendUint(uint64(p.Sum), 8, linewriter.AlignRight|linewriter.Hex|linewriter.WithZero)
	line.AppendBytes(bad, 8, linewriter.AlignCenter|linewriter.Text)
	if len(p.Data) > 0 {
		line.AppendUint(xxh.Sum64(p.Data, 0), 16, linewriter.AlignRight|linewriter.Hex|linewriter.WithZero)
	}
	os.Stdout.Write(append(line.Bytes(), '\n'))
}

func byChannel(p vmu.Packet) uint8 {
	return p.VMUHeader.Channel
}

func byOrigin(p vmu.Packet) uint8 {
	return p.DataHeader.Origin
}

func gapByChannel(p, prev vmu.Packet, duration time.Duration) (ok bool, g meex.Gap) {
	last, first := int(p.VMUHeader.Sequence), int(prev.VMUHeader.Sequence)
	delta := p.VMUHeader.Timestamp().Sub(prev.VMUHeader.Timestamp())

	if diff := last - first; diff != 1 && (duration == 0 || delta <= duration) {
		g.Last = first
		g.First = last
		g.Starts = prev.VMUHeader.Timestamp()
		g.Ends = p.VMUHeader.Timestamp()

		ok = true
	}
	return
}

func gapByOrigin(p, prev vmu.Packet, duration time.Duration) (ok bool, g meex.Gap) {
	last, first := int(p.DataHeader.Counter), int(prev.DataHeader.Counter)
	delta := p.DataHeader.Acquisition().Sub(prev.DataHeader.Acquisition())

	if diff := last - first; diff != 1 && (duration == 0 || delta <= duration) {
		g.Last = first
		g.First = last
		g.Starts = prev.DataHeader.Acquisition()
		g.Ends = p.DataHeader.Acquisition()

		ok = true
	}
	return
}

func whichChannel(c uint8) []byte {
	switch c {
	case vmu.VIC1:
		return chanVic1
	case vmu.VIC2:
		return chanVic2
	case vmu.LRSD:
		return chanLRSD
	default:
		return unknown
	}
}

func whichMode(rt bool) []byte {
	if rt {
		return modeRT
	}
	return modePB
}
