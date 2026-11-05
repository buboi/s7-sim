package main

import "time"

// FrameSource produces frames for the simulation loop. Next returns the frame
// to apply now and how long to wait before calling Next again. It returns
// io.EOF when the source is exhausted.
type FrameSource interface {
	Next() (Frame, time.Duration, error)
	Close() error
}

// jsonSource rotates through the frames of a JSON sample file.
type jsonSource struct {
	sample SampleData
	idx    int
}

func newJSONSource(sample SampleData) *jsonSource {
	return &jsonSource{sample: sample}
}

func (j *jsonSource) Next() (Frame, time.Duration, error) {
	frame := j.sample.Frames[j.idx]
	j.idx = (j.idx + 1) % len(j.sample.Frames)
	wait := frame.WaitMS
	if wait <= 0 {
		wait = j.sample.IntervalMS
	}
	if wait <= 0 {
		wait = 2000
	}
	return frame, time.Duration(wait) * time.Millisecond, nil
}

func (j *jsonSource) Close() error { return nil }
