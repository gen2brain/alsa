## ALSA
[![Status](https://github.com/gen2brain/alsa/actions/workflows/test.yml/badge.svg)](https://github.com/gen2brain/alsa/actions)
[![Go Reference](https://pkg.go.dev/badge/github.com/gen2brain/alsa.svg)](https://pkg.go.dev/github.com/gen2brain/alsa)

Go reimplementation of [tinyalsa](https://github.com/tinyalsa/tinyalsa) library.

This library provides a Go interface to the [Linux ALSA](https://en.wikipedia.org/wiki/Advanced_Linux_Sound_Architecture) for interacting with sound card devices,
allowing for raw audio (PCM) playback and capture, and control over mixer elements like volume and switches.

### Usage

See [utils](cmd/) for usage examples.

### Testing

Running the tests requires specific kernel modules to create virtual sound card devices for 
playback, capture, and mixer control tests without needing physical hardware:

 - `snd-dummy`: Creates a virtual sound card with a mixer, used for testing control functionality.
 - `snd-aloop`: Creates a loopback sound card, allowing playback data to be captured, which is essential for testing PCM I/O.

You can load them with the following commands:
```bash
sudo modprobe snd-dummy
sudo modprobe snd-aloop
```
