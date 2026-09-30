package transfer

import (
	"sync"
	"sync/atomic"
	"time"

	uatomic "go.uber.org/atomic"
)

// State of the current download.
type State int32

const (
	StateIdle State = iota
	StateConnecting
	StateDownloading
	StateRetryWait
	StateFinishing
	StateDone
)

// Progress is written by the downloader and read by the UI thread, so every
// field is atomic.
type Progress struct {
	name  atomic.Value // string
	Done  atomic.Int64 // bytes of the current file on disk, including resumed part
	Total atomic.Int64 // size of the current file, -1 if unknown
	Speed atomic.Int64 // bytes per second, smoothed
	state atomic.Int32

	Attempt atomic.Int32 // retry number in the current failure streak (0 = none)
	Retries atomic.Int32 // retries allowed per streak
	RetryAt atomic.Int64 // unix nanos of the next retry while in StateRetryWait

	// Fraction mirrors Done/Total for gabagool's progress bar.
	Fraction *uatomic.Float64

	mu        sync.Mutex
	lastErr   error
	sampleAt  time.Time
	sampleVal int64
}

// NewProgress returns a zeroed progress.
func NewProgress() *Progress {
	p := &Progress{Fraction: uatomic.NewFloat64(0)}
	p.name.Store("")
	p.Total.Store(-1)
	return p
}

// Name of the file being downloaded.
func (p *Progress) Name() string { return p.name.Load().(string) }

// State returns the current state.
func (p *Progress) State() State { return State(p.state.Load()) }

// LastError is the error that triggered the current retry wait.
func (p *Progress) LastError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastErr
}

// ETA estimates the remaining time; -1 if unknown.
func (p *Progress) ETA() time.Duration {
	total, done, speed := p.Total.Load(), p.Done.Load(), p.Speed.Load()
	if total < 0 || speed <= 0 || done > total {
		return -1
	}
	return time.Duration(float64(total-done) / float64(speed) * float64(time.Second))
}

func (p *Progress) start(name string, total, done int64) {
	p.name.Store(name)
	p.Total.Store(total)
	p.Done.Store(done)
	p.Speed.Store(0)
	p.Attempt.Store(0)
	p.setState(StateConnecting)
	p.mu.Lock()
	p.lastErr = nil
	p.sampleAt = time.Now()
	p.sampleVal = done
	p.mu.Unlock()
	p.updateFraction()
}

func (p *Progress) setState(s State) { p.state.Store(int32(s)) }

func (p *Progress) setRetry(err error, at time.Time) {
	p.mu.Lock()
	p.lastErr = err
	p.mu.Unlock()
	p.RetryAt.Store(at.UnixNano())
	p.Speed.Store(0)
	p.setState(StateRetryWait)
}

// setDone records bytes on disk; resets the speed sampler on jumps back.
func (p *Progress) setDone(done int64) {
	p.Done.Store(done)
	p.mu.Lock()
	p.sampleAt, p.sampleVal = time.Now(), done
	p.mu.Unlock()
	p.updateFraction()
}

// add accounts for n new bytes and refreshes the smoothed speed about twice
// a second (exponential moving average, so one slow chunk does not make the
// ETA jump around).
func (p *Progress) add(n int64) {
	done := p.Done.Add(n)
	p.mu.Lock()
	now := time.Now()
	if dt := now.Sub(p.sampleAt); dt >= 500*time.Millisecond {
		inst := float64(done-p.sampleVal) / dt.Seconds()
		old := float64(p.Speed.Load())
		speed := inst
		if old > 0 {
			speed = 0.7*old + 0.3*inst
		}
		p.Speed.Store(int64(speed))
		p.sampleAt, p.sampleVal = now, done
	}
	p.mu.Unlock()
	p.updateFraction()
}

func (p *Progress) updateFraction() {
	total := p.Total.Load()
	if total <= 0 {
		p.Fraction.Store(0)
		return
	}
	f := float64(p.Done.Load()) / float64(total)
	if f > 1 {
		f = 1
	}
	p.Fraction.Store(f)
}
