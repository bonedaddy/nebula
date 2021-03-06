package nebula

import (
	"github.com/rcrowley/go-metrics"
	"go.uber.org/zap"
)

type Bits struct {
	length             uint64
	current            uint64
	bits               []bool
	firstSeen          bool
	lostCounter        metrics.Counter
	dupeCounter        metrics.Counter
	outOfWindowCounter metrics.Counter
}

func NewBits(bits uint64) *Bits {
	return &Bits{
		length:             bits,
		bits:               make([]bool, bits),
		current:            0,
		lostCounter:        metrics.GetOrRegisterCounter("network.packets.lost", nil),
		dupeCounter:        metrics.GetOrRegisterCounter("network.packets.duplicate", nil),
		outOfWindowCounter: metrics.GetOrRegisterCounter("network.packets.out_of_window", nil),
	}
}

func (b *Bits) Check(i uint64) bool {
	// If i is the next number, return true.
	if i > b.current || (i == 0 && !b.firstSeen && b.current < b.length) {
		return true
	}

	// If i is within the window, check if it's been set already. The first window will fail this check
	if i > b.current-b.length {
		return !b.bits[i%b.length]
	}

	// If i is within the first window
	if i < b.length {
		return !b.bits[i%b.length]
	}

	// Not within the window
	l.Sugar().Debugf("rejected a packet (top) %d %d\n", b.current, i)
	return false
}

func (b *Bits) Update(i uint64) bool {
	// If i is the next number, return true and update current.
	if i == b.current+1 {
		// Report missed packets, we can only understand what was missed after the first window has been gone through
		if i > b.length && !b.bits[i%b.length] {
			b.lostCounter.Inc(1)
		}
		b.bits[i%b.length] = true
		b.current = i
		return true
	}

	// If i packet is greater than current but less than the maximum length of our bitmap,
	// flip everything in between to false and move ahead.
	if i > b.current && i < b.current+b.length {
		// In between current and i need to be zero'd to allow those packets to come in later
		for n := b.current + 1; n < i; n++ {
			b.bits[n%b.length] = false
		}

		b.bits[i%b.length] = true
		b.current = i
		//l.Debugf("missed %d packets between %d and %d\n", i-b.current, i, b.current)
		return true
	}

	// If i is greater than the delta between current and the total length of our bitmap,
	// just flip everything in the map and move ahead.
	if i >= b.current+b.length {
		// The current window loss will be accounted for later, only record the jump as loss up until then
		lost := maxInt64(0, int64(i-b.current-b.length))
		//TODO: explain this
		if b.current == 0 {
			lost++
		}

		for n := range b.bits {
			// Don't want to count the first window as a loss
			//TODO: this is likely wrong, we are wanting to track only the bit slots that we aren't going to track anymore and this is marking everything as missed
			//if b.bits[n] == false {
			//	lost++
			//}
			b.bits[n] = false
		}

		b.lostCounter.Inc(lost)
		l.Debug(
			"receive window",
			zap.Bool("accepted", true),
			zap.Uint64("currentCounter", b.current),
			zap.Uint64("incomingCounter", i),
			zap.String("reason", "window shifting"),
		)
		b.bits[i%b.length] = true
		b.current = i
		return true
	}

	// Allow for the 0 packet to come in within the first window
	if i == 0 && !b.firstSeen && b.current < b.length {
		b.firstSeen = true
		b.bits[i%b.length] = true
		return true
	}

	// If i is within the window of current minus length (the total pat window size),
	// allow it and flip to true but to NOT change current. We also have to account for the first window
	if ((b.current >= b.length && i > b.current-b.length) || (b.current < b.length && i < b.length)) && i <= b.current {
		if b.current == i {
			l.Debug(
				"receive window",
				zap.Bool("accepted", false),
				zap.Uint64("currentCounter", b.current),
				zap.Uint64("incomingCounter", i),
				zap.String("reason", "duplicate"),
			)
			b.dupeCounter.Inc(1)
			return false
		}

		if b.bits[i%b.length] {
			l.Debug(
				"receive window",
				zap.Bool("accepted", false),
				zap.Uint64("currentCounter", b.current),
				zap.Uint64("incomingCounter", i),
				zap.String("reason", "duplicate"),
			)
			b.dupeCounter.Inc(1)
			return false
		}

		b.bits[i%b.length] = true
		return true

	}

	// In all other cases, fail and don't change current.
	b.outOfWindowCounter.Inc(1)
	l.Debug(
		"receive window",
		zap.Bool("accepted", false),
		zap.Uint64("currentCounter", b.current),
		zap.Uint64("incomingCounter", i),
		zap.String("reason", "nonsense"),
	)
	return false
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}

	return b
}
