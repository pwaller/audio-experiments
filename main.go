package main

import (
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"sync/atomic"
	"time"
	"unsafe"

	"net/http"
	_ "net/http/pprof"

	al "azul3d.org/native/al.v1-unstable"
)

var bufCalls int32

func main() {

	go http.ListenAndServe(":8081", nil)

	const (
		Frequency  = 44100
		BufSamples = Frequency / 25
	)

	a, err := NewAudio(Frequency, BufSamples)
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()

	go a.Run()

	input := a.Input()

	step := 0

	d := a.bufferDuration()

	go func() {
		for range time.NewTicker(time.Second).C {
			bufCalls := atomic.SwapInt32(&bufCalls, 0)
			log.Println("Buftime:", time.Duration(bufCalls)*d)
		}
	}()

	log.Println("Buf duration: ", d)

	tick := time.NewTicker(d).C

	mkSamples := func() {
		for i := 0; i < BufSamples; i++ {
			t := 10 * float64(step) / Frequency

			amp := math.Sin(220 * t)

			amp *= math.MaxInt16

			input <- int16(amp)
			step += 1
		}
	}

	for {
		mkSamples()
		// <-tick
		_ = tick
	}

	select {}
}

type Azul3DAudio struct {
	paused     bool
	frequency  int
	speed      float32
	device     *al.Device
	source     uint32
	buffers    []uint32
	sampleSize int
	input      chan int16
}

func NewAudio(frequency int, sampleSize int) (audio *Azul3DAudio, err error) {
	var device *al.Device

	device, err = al.OpenDevice("", nil)

	if err != nil {
		return
	}

	audio = &Azul3DAudio{
		frequency:  frequency,
		speed:      1.0,
		device:     device,
		sampleSize: sampleSize,
		buffers:    make([]uint32, 2),
		input:      make(chan int16, sampleSize),
	}

	al.SetErrorHandler(func(e error) {
		err = e
	})

	device.GenSources(1, &audio.source)

	if !device.IsSource(audio.source) {
		err = errors.New("IsSource returned false")
		return
	}

	device.Sourcei(audio.source, al.LOOPING, al.FALSE)

	device.GenBuffers(int32(len(audio.buffers)), &audio.buffers[0])

	for i := range audio.buffers {
		if !device.IsBuffer(audio.buffers[i]) {
			err = errors.New(fmt.Sprintf("IsBuffer[%v] returned false", i))
			return
		}
	}

	return
}

func (audio *Azul3DAudio) Input() chan int16 {
	return audio.input
}

func (audio *Azul3DAudio) bufferDuration() (duration time.Duration) {
	duration = time.Millisecond *
		time.Duration(1000*(float32(audio.sampleSize)/(float32(audio.frequency)*audio.speed)))
	return duration
}

func (audio *Azul3DAudio) stream(schan chan []int16) {

	samples := []int16{}

	for {
		samples = append(samples, <-audio.input)

		if len(samples) == audio.sampleSize {
			schan <- samples
			samples = []int16{}
		}
	}
}

func (audio *Azul3DAudio) bufferData(buffer uint32, samples []int16) (err error) {

	atomic.AddInt32(&bufCalls, 1)

	al.SetErrorHandler(func(e error) {
		err = e
	})

	audio.device.BufferData(buffer, al.FORMAT_MONO16, unsafe.Pointer(&samples[0]),
		int32(int(unsafe.Sizeof(samples[0]))*len(samples)), int32(float32(audio.frequency)*audio.speed))

	return
}

func (audio *Azul3DAudio) Run() {
	running := true

	handler := func(e error) {
		if e != nil {
			fmt.Fprintf(os.Stderr, "%v\n", e)
			running = false
		}
	}

	al.SetErrorHandler(handler)

	schan := make(chan []int16, len(audio.buffers))

	go audio.stream(schan)

	audio.device.SourceQueueBuffers(audio.source, audio.buffers)
	audio.device.SourcePlay(audio.source)

	state := al.PLAYING
	queued := int32(len(audio.buffers))
	processed := int32(0)
	var samples []int16

	for running {
		// Wait for one audio buffer to be prepared.
		samples = <-schan

		// Wait for at least one buffer to be processed by OpenAL, so we can refill
		// it.
		for {
			audio.device.GetSourcei(audio.source, al.BUFFERS_PROCESSED, &processed)
			if processed > 0 {
				break
			}
		}

		// In situations where OpenAL runs out of buffers to play (e.g. if the app
		// stalled because the user's system was doing a lot of work), OpenAL will
		// stop the audio source from playing.
		//
		// We wait until each of the buffers are done playing in this case, so that
		// we may keep are playhead at the first buffer to keep our double
		// buffering.
		if audio.device.GetSourcei(audio.source, al.SOURCE_STATE, &state); state != al.PLAYING {
			fmt.Println("nes: Failed to feed audio to OpenAL fast enough; resynching...")
			for queued > 0 {
				audio.device.GetSourcei(audio.source, al.BUFFERS_PROCESSED, &processed)

				if processed == queued {
					break
				}
			}
		}

		// Dequeue the buffers that were processed by OpenAL.
		pbuffers := make([]uint32, processed)
		audio.device.SourceUnqueueBuffers(audio.source, pbuffers)

		// Fill each buffer with data and queue them again.
		for i := range pbuffers {
			if samples == nil {
				samples = <-schan
			}

			handler(audio.bufferData(pbuffers[i], samples))
			audio.device.SourceQueueBuffers(audio.source, []uint32{pbuffers[i]})
			samples = nil
		}

		audio.device.GetSourcei(audio.source, al.BUFFERS_QUEUED, &queued)

		// Begin playing the source now that we've filled all the buffers.
		if audio.device.GetSourcei(audio.source, al.SOURCE_STATE, &state); state != al.PLAYING {
			audio.device.SourcePlay(audio.source)
		}
	}
}

func (audio *Azul3DAudio) TogglePaused() {
	audio.paused = !audio.paused
}

func (audio *Azul3DAudio) SetSpeed(speed float32) {
	audio.speed = speed
}

func (audio *Azul3DAudio) Close() {
	audio.device.DeleteSources(1, &audio.source)
	audio.device.DeleteBuffers(int32(len(audio.buffers)), &audio.buffers[0])
	audio.device.Close()
}
