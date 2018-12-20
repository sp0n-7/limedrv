package main

import (
	"fmt"
	"github.com/myriadrf/limedrv"
	"github.com/racerxdl/segdsp/dsp"
	"github.com/racerxdl/segdsp/dsp/fft"
	"github.com/racerxdl/segdsp/tools"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"math"
	"os"
	"sync"
	"time"
)

const centerFreq = 90e6
const sampleRate = 20e6
const overSample = 2
const fftChunkSize = 16384
const numAverage = 64

var finishedWork chan bool
var dev *limedrv.LMSDevice

var samplesBuffer = make([][]complex64, numAverage)
var currentBuff = 0
var writeLock = sync.Mutex{}

func OnSamples(data []complex64, _ int, _ uint64) {
	writeLock.Lock()
	defer writeLock.Unlock()

	if currentBuff >= numAverage {
		return
	}

	samplesBuffer[currentBuff] = data[:fftChunkSize]
	currentBuff++
	if currentBuff == numAverage {
		finishedWork <- true
	}
}

func combine(c1, c2 color.Color) color.Color {
	r, g, b, a := c1.RGBA()
	r2, g2, b2, a2 := c2.RGBA()

	return color.RGBA{
		R: uint8((r + r2) >> 9), // div by 2 followed by ">> 8"  is ">> 9"
		G: uint8((g + g2) >> 9),
		B: uint8((b + b2) >> 9),
		A: uint8((a + a2) >> 9),
	}
}

func DrawLine(x0, y0, x1, y1 float32, color color.Color, img *image.RGBA) {
	// DDA
	_, _, _, a := color.RGBA()
	needsCombine := a != 255 && a != 0
	var dx = x1 - x0
	var dy = y1 - y0
	var steps float32
	if tools.Abs(dx) > tools.Abs(dy) {
		steps = tools.Abs(dx)
	} else {
		steps = tools.Abs(dy)
	}

	var xinc = dx / steps
	var yinc = dy / steps

	var x = x0
	var y = y0
	for i := 0; i < int(steps); i++ {
		if needsCombine {
			var p = img.At(int(x), int(y))
			img.Set(int(x), int(y), combine(p, color))
		} else {
			img.Set(int(x), int(y), color)
		}
		x = x + xinc
		y = y + yinc
	}
}

func main() {
	devices := limedrv.GetDevices()
	finishedWork = make(chan bool)

	log.Printf("Found %d devices.\n", len(devices))

	if len(devices) == 0 {
		log.Println("No devices found.")
		os.Exit(1)
	}

	if len(devices) > 1 {
		log.Println("More than one device found. Selecting first one.")
	}

	var di = devices[0]

	log.Printf("Opening device %s\n", di.DeviceName)

	dev = limedrv.Open(di)
	log.Println("Opened!")

	window := dsp.BlackmanHarris(fftChunkSize, 61)

	var ch = dev.RXChannels[limedrv.ChannelA]

	dev.SetSampleRate(sampleRate, overSample)

	ch.Enable().
		SetAntennaByName("LNAW").
		SetGainNormalized(0.5).
		SetLPF(sampleRate).
		EnableLPF().
		SetCenterFrequency(centerFreq)

	dev.SetCallback(OnSamples)

	dev.Start()

	log.Println("Waiting for work finish")
	<- finishedWork
	dev.Stop()
	dev.Close()

	log.Printf("Computing FFT\n")
	var fftDb = make([]float32, fftChunkSize)
	var fftMax = float32(-999999999)
	var fftMin = float32(9999999999)

	for _, v := range samplesBuffer {
		for j := 0; j < fftChunkSize; j++ {
			// Apply window to FFT
			var s = v[j]
			var r = real(s) * float32(window[j])
			var i = imag(s) * float32(window[j])
			v[j] = complex(r, i)
		}
		// Calculate FFT
		f := fft.FFT(v)

		for i := 0; i < len(f); i++ {
			// Convert FFT to Power in dB
			var v = tools.ComplexAbsSquared(f[i]) * (1.0 / sampleRate)
			v = float32(10 * math.Log10(float64(v)))
			fftDb[i] += v
		}
	}

	// Compute Average
	for i := range fftDb {
		fftDb[i] /= numAverage
		if fftDb[i] > fftMax {
			fftMax = fftDb[i]
		} else if fftDb[i] < fftMin {
			fftMin = fftDb[i]
		}
	}

	// Compute delta to make FFT always fit in the image
	var fftDelta = fftMax - fftMin

	log.Printf("Drawing FFT %dx%d\n", len(fftDb), 1024)
	img := image.NewRGBA(image.Rect(0, 0, len(fftDb), 1024))
	var size = img.Bounds()
	var lastX = float32(0)
	var lastY = float32(0)

	for i := 0; i < len(fftDb); i++ {
		var iPos = (i + len(fftDb)/2) % len(fftDb)
		var s = float32(fftDb[iPos])
		var v = (float32(fftMax) - s) * (float32(size.Dy()) / float32(fftDelta))
		var x = float32(i)
		if i != 0 {
			DrawLine(lastX, lastY, x, v, color.NRGBA{R: 0, G: 127, B: 127, A: 255}, img)
		}

		lastX = x
		lastY = v
	}

	log.Println("Saving file")
	// Encode as JPEG and Save
	filename := fmt.Sprintf("%d-%.0f-%.0f-fft.jpg", time.Now().Unix(), centerFreq, sampleRate)
	f, err := os.Create(filename)

	defer f.Close()
	if err != nil {
		panic(err)
	}

	err = jpeg.Encode(f, img, nil)
	if err != nil {
		panic(err)
	}
	log.Printf("File saved at %s\n", filename)
}