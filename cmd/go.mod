module github.com/gen2brain/alsa/cmd

go 1.23.0

replace github.com/gen2brain/alsa => ../

require (
	github.com/gen2brain/alsa v0.0.0-00010101000000-000000000000
	github.com/go-audio/audio v1.0.0
	github.com/go-audio/wav v1.1.0
	github.com/hajimehoshi/go-mp3 v0.3.4
)

require (
	github.com/go-audio/riff v1.0.0 // indirect
	golang.org/x/sys v0.35.0 // indirect
)
