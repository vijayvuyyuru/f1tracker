package f1viz

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/geo/r3"
	vizClient "github.com/viam-labs/motion-tools/client/client"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/utils"
)

var (
	F1viz      = resource.NewModel("vijayvuyyuru", "viz", "f1viz")
	startPoint = r3.Vector{X: -641,
		Y: -922,
		Z: 1303,
	}
)

type TrackPoint struct {
	X int `json:"x"`
	Y int `json:"y"`
	Z int `json:"z"`
}

// ReferenceTrack contains 144 points representing the track layout (one per index 0-143)
type ReferenceTrack struct {
	StartPoint TrackPoint   `json:"start_point"`
	Points     []TrackPoint `json:"points"` // 144 points, index 0-143
}

const (
	circuitKey  = 9
	sessionName = "Race"
	// Channel buffer size - adjust based on render speed vs fetch speed
	locationChannelBuffer = 500
	// Time window for each API fetch (30 seconds)
	fetchWindowDuration = time.Minute
	// Threshold to trigger next fetch when buffer drops below this percentage
	bufferLowThreshold = 0.2
	referenceTrackFile = "reference_track.json"
)

// Session represents a session from the OpenF1 API
type Session struct {
	SessionKey int    `json:"session_key"`
	DateStart  string `json:"date_start"`
	DateEnd    string `json:"date_end"`
}

// Location represents a location data point from the OpenF1 API
type Location struct {
	Date         string `json:"date"`
	DriverNumber int    `json:"driver_number"`
	MeetingKey   int    `json:"meeting_key"`
	SessionKey   int    `json:"session_key"`
	X            int    `json:"x"`
	Y            int    `json:"y"`
	Z            int    `json:"z"`
}

func init() {
	resource.RegisterService(generic.API, F1viz,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newVizF1viz,
		},
	)
}

type Config struct {
	Board string `json:"board"`
}

// Validate ensures all parts of the config are valid and important fields exist.
// Returns three values:
//  1. Required dependencies: other resources that must exist for this resource to work.
//  2. Optional dependencies: other resources that may exist but are not required.
//  3. An error if any Config fields are missing or invalid.
//
// The `path` parameter indicates
// where this resource appears in the machine's JSON configuration
// (for example, "components.0"). You can use it in error messages
// to indicate which resource has a problem.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	// Add config validation code here
	return nil, nil, nil
}

type vizF1viz struct {
	resource.AlwaysRebuild

	name resource.Name

	logger logging.Logger
	cfg    *Config

	cancelCtx  context.Context
	cancelFunc func()

	referenceTrack ReferenceTrack

	// For producer-consumer pattern
	locationChans []chan Location // One channel per driver
	workers       *utils.StoppableWorkers
	started       atomic.Bool

	// Timestamp tracking
	timestampData []RoundTimestamp
	timestampMu   sync.Mutex
	roundCounter  int64
}

// RoundTimestamp represents a single round of location data collection
type RoundTimestamp struct {
	Round     int64               `json:"round"`
	Timestamp string              `json:"timestamp"` // When this round was collected
	Drivers   map[int]DriverStamp `json:"drivers"`   // Driver number -> timestamp
}

// DriverStamp contains timestamp data for a single driver
type DriverStamp struct {
	DriverNumber int    `json:"driver_number"`
	Timestamp    string `json:"timestamp"` // From Location.Date
}

func newVizF1viz(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	return NewF1viz(ctx, deps, rawConf.ResourceName(), conf, logger)

}

func NewF1viz(ctx context.Context, deps resource.Dependencies, name resource.Name, conf *Config, logger logging.Logger) (resource.Resource, error) {

	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	s := &vizF1viz{
		name:       name,
		logger:     logger,
		cfg:        conf,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
		started:    atomic.Bool{},
	}

	referenceTrack, err := loadReferenceTrack()
	if err != nil {
		return nil, fmt.Errorf("failed to load reference track: %w", err)
	}
	s.referenceTrack = referenceTrack
	return s, nil
}

func (s *vizF1viz) Name() resource.Name {
	return s.name
}

func (s *vizF1viz) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	var commandKey string
	for k := range cmd {
		commandKey = k
	}
	switch commandKey {
	case "draw_reference_track":
		err := s.drawReferenceTrack()
		if err != nil {
			return nil, fmt.Errorf("failed to draw reference track: %w", err)
		}
		return map[string]interface{}{
			"status": "success",
		}, nil
	case "start":
		s.drawReferenceTrack()
		return s.start(ctx, cmd[commandKey])
	case "stop":
		s.workers.Stop()
		s.workers = utils.NewStoppableWorkers(s.cancelCtx)
		// Write timestamps to disk
		if err := s.writeTimestampsToDisk(); err != nil {
			s.logger.Errorf("Failed to write timestamps to disk: %v", err)
			return nil, err
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown command: %s", commandKey)
	}
}

func (s *vizF1viz) start(ctx context.Context, cmdValue interface{}) (map[string]interface{}, error) {
	if !s.started.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("already started")
	}

	// Parse driver numbers from command
	var driverNumbers []int

	// Handle []int directly
	if nums, ok := cmdValue.([]int); ok {
		driverNumbers = nums
	} else if nums, ok := cmdValue.([]interface{}); ok {
		// Handle []interface{} from JSON parsing
		driverNumbers = make([]int, 0, len(nums))
		for i, v := range nums {
			var num int
			switch n := v.(type) {
			case int:
				num = n
			case int64:
				num = int(n)
			case float64:
				num = int(n)
			default:
				return nil, fmt.Errorf("start command: element at index %d is not a number, got %T", i, v)
			}
			driverNumbers = append(driverNumbers, num)
		}
	} else {
		return nil, fmt.Errorf("start command expects a list of integers, got %T", cmdValue)
	}

	if len(driverNumbers) == 0 {
		return nil, fmt.Errorf("start command requires at least one driver number")
	}

	s.logger.Infof("Starting with driver numbers: %v", driverNumbers)

	// Fetch session first
	session, err := s.fetchSession(ctx)
	if err != nil {
		s.started.CompareAndSwap(true, false)
		return nil, fmt.Errorf("failed to fetch session: %w", err)
	}
	sessionKey := session.SessionKey
	s.logger.Infof("Using session_key: %d", sessionKey)

	// Parse start time
	startTime, err := time.Parse(time.RFC3339, session.DateStart)
	if err != nil {
		s.started.CompareAndSwap(true, false)
		return nil, fmt.Errorf("failed to parse session start time: %w", err)
	}

	// Create buffered channel for each driver
	s.locationChans = make([]chan Location, len(driverNumbers))
	for i := range s.locationChans {
		s.locationChans[i] = make(chan Location, locationChannelBuffer)
	}

	// Create StoppableWorkers using cancelCtx
	s.workers = utils.NewStoppableWorkers(s.cancelCtx)

	// Create a fetcher worker for each driver
	for i, driverNumber := range driverNumbers {
		driverNum := driverNumber // Capture for closure
		driverChan := s.locationChans[i]

		// Create fetcher state for this driver
		state := &fetcherState{
			sessionKey:      sessionKey,
			lastFetchedTime: startTime,
			driverNumber:    driverNum,
		}

		s.logger.Infof("Starting fetcher for driver %d, session %d, starting from %s", driverNum, sessionKey, startTime.Format(time.RFC3339))

		// Create fetcher worker with ticker (checks buffer and fetches every 1 second)
		s.workers.Add(func(ctx context.Context) {
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			defer close(driverChan)

			for {
				select {
				case <-ctx.Done():
					s.logger.Infof("Fetcher for driver %d cancelled", driverNum)
					return
				case <-ticker.C:
					s.fetcher(ctx, state, driverChan)
				}
			}
		})
	}

	// Create consumer worker
	s.workers.Add(func(ctx context.Context) {
		defer s.started.CompareAndSwap(true, false)
		s.consumer(ctx)
	})

	// Return immediately - workers run in background
	return map[string]interface{}{
		"status":  "started",
		"message": fmt.Sprintf("Fetcher and consumer workers started for %d drivers", len(driverNumbers)),
	}, nil
}

// fetcherState holds state for the fetcher worker
type fetcherState struct {
	sessionKey      int
	lastFetchedTime time.Time
	driverNumber    int
}

// fetcher is the work function called by the ticker-based fetcher worker
func (s *vizF1viz) fetcher(ctx context.Context, state *fetcherState, driverChan chan Location) {
	// Check buffer level
	bufferLevel := float64(len(driverChan)) / float64(cap(driverChan))
	if bufferLevel < bufferLowThreshold {
		// Fetch next window
		endTime := state.lastFetchedTime.Add(fetchWindowDuration)
		locations, err := s.fetchLocationData(ctx, state.sessionKey, state.driverNumber, state.lastFetchedTime, endTime)
		if err != nil {
			s.logger.Errorf("Failed to fetch location data for driver %d: %v", state.driverNumber, err)
			// Continue - don't exit on error, just retry next tick
			return
		}

		if len(locations) == 0 {
			// No more data available for this driver - close its channel
			s.logger.Infof("No more location data available for driver %d, closing channel", state.driverNumber)
			close(driverChan)
			return
		}

		// Send locations to channel
		for _, loc := range locations {
			select {
			case <-ctx.Done():
				return
			case driverChan <- loc:
				// Successfully sent
			}
		}

		// Update lastFetchedTime to the last location's time + small increment
		// to avoid fetching the same point again (using >= in query)
		if len(locations) > 0 {
			lastLocTime, err := time.Parse(time.RFC3339, locations[len(locations)-1].Date)
			if err == nil {
				// Add 1ms to avoid re-fetching the last point
				state.lastFetchedTime = lastLocTime.Add(1 * time.Millisecond)
			} else {
				// If parsing fails, advance by window duration
				state.lastFetchedTime = endTime
			}
		} else {
			state.lastFetchedTime = endTime
		}

		s.logger.Debugf("Fetched %d locations for driver %d, buffer level: %.2f%%", len(locations), state.driverNumber, bufferLevel*100)
	}
}

// consumer continuously consumes and renders location data from all channels
func (s *vizF1viz) consumer(ctx context.Context) {
	s.logger.Info("Consumer started, waiting for location data from all drivers...")

	// Track locations per driver for trail rendering
	locationHistories := make(map[int][]Location)
	trailLength := 5

	// Track which channels are still open
	openChannels := make(map[int]bool)
	for i := range s.locationChans {
		openChannels[i] = true
	}

	// Outer loop: continue until all channels are closed
	for len(openChannels) > 0 {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			s.logger.Info("Consumer cancelled")
			return
		default:
		}

		// Collect one location from each open channel
		currentLocations := make(map[int]Location)

		// Iterate through each channel and read from it
		for i, ch := range s.locationChans {
			if !openChannels[i] {
				continue
			}

			// Read from this channel (blocking)
			select {
			case <-ctx.Done():
				s.logger.Info("Consumer cancelled")
				return
			case location, ok := <-ch:
				if !ok {
					// Channel closed for this driver
					s.logger.Infof("Channel closed for driver index %d", i)
					delete(openChannels, i)
					continue
				}
				currentLocations[i] = location
			}
		}

		// If we have locations from all open channels, render
		if len(currentLocations) == len(openChannels) && len(currentLocations) > 0 {
			// Record timestamps for this round
			round := atomic.AddInt64(&s.roundCounter, 1)
			roundTimestamp := RoundTimestamp{
				Round:     round,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Drivers:   make(map[int]DriverStamp),
			}
			for _, location := range currentLocations {
				roundTimestamp.Drivers[location.DriverNumber] = DriverStamp{
					DriverNumber: location.DriverNumber,
					Timestamp:    location.Date,
				}
			}

			// Save timestamp data
			s.timestampMu.Lock()
			s.timestampData = append(s.timestampData, roundTimestamp)
			s.timestampMu.Unlock()

			// Update histories for all drivers
			for _, location := range currentLocations {
				history := locationHistories[location.DriverNumber]
				history = append(history, location)
				if len(history) > trailLength {
					history = history[len(history)-trailLength:]
				}
				locationHistories[location.DriverNumber] = history
			}

			// Render all locations as one pointcloud
			if err := s.renderLocations(currentLocations, locationHistories); err != nil {
				s.logger.Errorf("Failed to render locations: %v", err)
				// Continue rendering even if one fails
			}

			// Small delay to control render rate
			time.Sleep(10 * time.Millisecond)
		}
	}

	s.logger.Info("All channels closed, consumer stopping")
}

// fetchSession fetches session information from OpenF1 API
func (s *vizF1viz) fetchSession(ctx context.Context) (Session, error) {
	baseURL := "https://api.openf1.org/v1/sessions"
	u, err := url.Parse(baseURL)
	if err != nil {
		return Session{}, fmt.Errorf("failed to parse URL: %w", err)
	}

	q := u.Query()
	q.Set("circuit_key", fmt.Sprintf("%d", circuitKey))
	q.Set("session_name", sessionName)
	q.Set("year", "2023")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return Session{}, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Session{}, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Session{}, fmt.Errorf("failed to read response: %w", err)
	}

	var sessions []Session
	if err := json.Unmarshal(body, &sessions); err != nil {
		return Session{}, fmt.Errorf("failed to parse sessions: %w", err)
	}

	if len(sessions) == 0 {
		return Session{}, fmt.Errorf("no sessions found")
	}

	return sessions[0], nil
}

// fetchLocationData fetches location data for a given time window
func (s *vizF1viz) fetchLocationData(ctx context.Context, sessionKey, driverNumber int, startTime, endTime time.Time) ([]Location, error) {
	locationURL := "https://api.openf1.org/v1/location"
	u, err := url.Parse(locationURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse location URL: %w", err)
	}

	// Format times for API (OpenF1 expects format: 2006-01-02T15:04:05.000)
	startTimeStr := startTime.UTC().Format("2006-01-02T15:04:05.000")
	endTimeStr := endTime.UTC().Format("2006-01-02T15:04:05.000")

	startEncoded := url.QueryEscape(startTimeStr)
	endEncoded := url.QueryEscape(endTimeStr)

	// Use date>= for start to include boundary (for pagination continuity)
	// Use date< for end to exclude boundary (matches OpenF1 API format)
	queryString := fmt.Sprintf("session_key=%d&driver_number=%d&date>=%s&date<%s",
		sessionKey, driverNumber, startEncoded, endEncoded)
	u.RawQuery = queryString

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var locations []Location
	if err := json.Unmarshal(body, &locations); err != nil {
		return nil, fmt.Errorf("failed to parse locations: %w", err)
	}

	return locations, nil
}

// renderLocations renders locations from all drivers as one pointcloud
func (s *vizF1viz) renderLocations(currentLocations map[int]Location, locationHistories map[int][]Location) error {
	pc := pointcloud.NewBasicEmpty()

	// Render each driver's current location and trail
	for _, location := range currentLocations {
		history := locationHistories[location.DriverNumber]

		// Render trail with fading intensity
		for i, loc := range history {
			// Calculate fade factor: most recent is 1.0, oldest fades to 0.0
			fadeFactor := float64(i) / float64(len(history)-1)
			if len(history) == 1 {
				fadeFactor = 1.0
			}

			// Color: different bright colors per driver, with fading
			// Predefined palette of distinct bright colors for up to 10 drivers
			driverColors := [][]uint8{
				{255, 0, 0},     // Red
				{0, 255, 0},     // Green
				{0, 0, 255},     // Blue
				{255, 255, 0},   // Yellow
				{255, 0, 255},   // Magenta
				{0, 255, 255},   // Cyan
				{255, 128, 0},   // Orange
				{128, 0, 255},   // Purple
				{255, 192, 203}, // Pink
				{0, 255, 128},   // Spring Green
			}

			driverIdx := location.DriverNumber % 10
			baseColor := driverColors[driverIdx]

			// Apply fade factor to make trail fade
			r := uint8(float64(baseColor[0]) * fadeFactor)
			g := uint8(float64(baseColor[1]) * fadeFactor)
			b := uint8(float64(baseColor[2]) * fadeFactor)

			pc.Set(r3.Vector{
				X: float64(loc.X),
				Y: float64(loc.Y),
				Z: float64(loc.Z),
			}, pointcloud.NewColoredData(color.NRGBA{R: r, G: g, B: b, A: 255}))
		}
	}

	// Log rendering info
	driverNums := make([]int, 0, len(currentLocations))
	for _, loc := range currentLocations {
		driverNums = append(driverNums, loc.DriverNumber)
	}
	s.logger.Debugf("Rendering pointcloud with %d drivers: %v", len(currentLocations), driverNums)

	// Render the complete pointcloud
	vizClient.DrawPointCloud("movement", pc, nil)

	return nil
}

func (s *vizF1viz) Close(context.Context) error {
	s.cancelFunc()
	if s.workers != nil {
		s.workers.Stop()
	}
	// Write timestamps to disk on close
	if err := s.writeTimestampsToDisk(); err != nil {
		s.logger.Errorf("Failed to write timestamps to disk on close: %v", err)
	}
	return nil
}

// writeTimestampsToDisk writes the collected timestamp data to a JSON file
func (s *vizF1viz) writeTimestampsToDisk() error {
	s.timestampMu.Lock()
	defer s.timestampMu.Unlock()

	if len(s.timestampData) == 0 {
		s.logger.Info("No timestamp data to write")
		return nil
	}

	// Create filename with timestamp
	filename := fmt.Sprintf("timestamps_%s.json", time.Now().UTC().Format("20060102_150405"))

	data, err := json.MarshalIndent(s.timestampData, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal timestamp data: %w", err)
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		return fmt.Errorf("failed to write timestamp file: %w", err)
	}

	// Get absolute path for logging
	absPath, err := filepath.Abs(filename)
	if err != nil {
		absPath = filename // Fallback to relative path if absolute fails
	}

	s.logger.Infof("Wrote %d rounds of timestamp data to %s", len(s.timestampData), absPath)

	// Clear the data after writing
	s.timestampData = nil
	s.roundCounter = 0

	return nil
}

// loadReferenceTrack loads a reference track from a JSON file
func loadReferenceTrack() (ReferenceTrack, error) {
	data, err := os.ReadFile(referenceTrackFile)
	if err != nil {
		return ReferenceTrack{}, err
	}

	var track ReferenceTrack
	err = json.Unmarshal(data, &track)
	if err != nil {
		return ReferenceTrack{}, err
	}

	if len(track.Points) != 144 {
		return ReferenceTrack{}, fmt.Errorf("reference track must have exactly 144 points, got %d", len(track.Points))
	}

	return track, nil
}

func (s *vizF1viz) drawReferenceTrack() error {
	pc := pointcloud.NewBasicEmpty()

	// Draw all 144 points from the reference track
	for i, point := range s.referenceTrack.Points {
		// Color based on index: gradient from blue (0) to red (143)
		r := uint8((i * 255) / 143)
		b := uint8(255 - (i * 255 / 143))

		pc.Set(r3.Vector{
			X: float64(point.X),
			Y: float64(point.Y),
			Z: float64(point.Z),
		}, pointcloud.NewColoredData(color.NRGBA{R: r, G: 0, B: b, A: 255}))
	}

	return vizClient.DrawPointCloud("reference", pc, nil)
}
