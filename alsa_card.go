package alsa

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// SoundCardDevice represents a single PCM device on a sound card.
type SoundCardDevice struct {
	ID          int
	Name        string
	Description string
	IsPlayback  bool // True for playback, false for capture
}

// String returns a human-readable representation of the SoundCardDevice.
func (d SoundCardDevice) String() string {
	direction := "Capture"
	if d.IsPlayback {
		direction = "Playback"
	}

	return fmt.Sprintf("  Device %d: %s (%s) [%s]", d.ID, d.Name, d.Description, direction)
}

// SoundCard represents an enumerated sound card with its devices.
type SoundCard struct {
	ID          int
	Name        string
	Description string
	Devices     []SoundCardDevice
}

// String returns a human-readable representation of the SoundCard.
func (c SoundCard) String() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Card %d: %s (%s)\n", c.ID, c.Name, c.Description))
	for _, dev := range c.Devices {
		sb.WriteString(dev.String() + "\n")
	}

	return sb.String()
}

// EnumerateCards scans /proc/asound to find all available sound cards and their PCM devices.
func EnumerateCards() ([]SoundCard, error) {
	cardsFile := "/proc/asound/cards"
	cardsContent, err := os.ReadFile(cardsFile)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", cardsFile, err)
	}

	cardMap := make(map[int]*SoundCard)
	cardRegex := regexp.MustCompile(`^\s*(\d+)\s+\[\s*([^]]*?)\s*\]:\s*(.*)`)
	lines := strings.Split(string(cardsContent), "\n")

	for _, line := range lines {
		matches := cardRegex.FindStringSubmatch(line)
		if len(matches) == 4 {
			id, err := strconv.Atoi(matches[1])
			if err != nil {
				continue
			}
			card := &SoundCard{
				ID:          id,
				Name:        strings.TrimSpace(matches[2]),
				Description: strings.TrimSpace(matches[3]),
			}
			cardMap[id] = card
		}
	}

	pcmFile := "/proc/asound/pcm"
	pcmContent, err := os.ReadFile(pcmFile)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", pcmFile, err)
	}

	pcmLines := strings.Split(string(pcmContent), "\n")
	// Regex to parse lines like "02-00: Loopback PCM : Loopback PCM : playback 8 : capture 8"
	pcmRegex := regexp.MustCompile(`^(\d+)-(\d+): (.*?) :.*`)

	for _, line := range pcmLines {
		matches := pcmRegex.FindStringSubmatch(line)
		if len(matches) < 4 {
			continue
		}

		cardID, _ := strconv.Atoi(matches[1])
		devID, _ := strconv.Atoi(matches[2])

		card, ok := cardMap[cardID]
		if !ok {
			continue
		}

		description := strings.TrimSpace(matches[3])

		// A single PCM device can have both playback and capture streams.
		// We create a distinct SoundCardDevice for each capability found.
		if strings.Contains(line, "playback") {
			device := SoundCardDevice{
				ID:          devID,
				Name:        fmt.Sprintf("pcm%dp", devID),
				Description: description,
				IsPlayback:  true,
			}

			card.Devices = append(card.Devices, device)
		}

		if strings.Contains(line, "capture") {
			device := SoundCardDevice{
				ID:          devID,
				Name:        fmt.Sprintf("pcm%dc", devID),
				Description: description,
				IsPlayback:  false,
			}

			card.Devices = append(card.Devices, device)
		}
	}

	var result []SoundCard
	var cardIDs []int
	for id := range cardMap {
		cardIDs = append(cardIDs, id)
	}

	sort.Ints(cardIDs)

	for _, id := range cardIDs {
		result = append(result, *cardMap[id])
	}

	return result, nil
}
