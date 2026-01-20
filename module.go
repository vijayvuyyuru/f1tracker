package f1viz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/utils"
)

var (
	F1viz            = resource.NewModel("vijayvuyyuru", "viz", "f1viz")
	errUnimplemented = errors.New("unimplemented")
	startPoint       = r3.Vector{X: -641,
		Y: -922,
		Z: 1303,
	}
)

const (
	circuitKey  = 9
	sessionName = "Race"
	// Channel buffer size - adjust based on render speed vs fetch speed
	locationChannelBuffer = 500
	// Time window for each API fetch (30 seconds)
	fetchWindowDuration = 30 * time.Second
	// Threshold to trigger next fetch when buffer drops below this percentage
	bufferLowThreshold = 0.2
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

	// For producer-consumer pattern
	locationChan chan Location
	workers      *utils.StoppableWorkers
	started      atomic.Bool
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
		// referenceTrack, err := loadReferenceTrack("reference_track.json")
		// if err != nil {
		// 	return nil, err
		// }
		// drawReferenceTrack(referenceTrack)
		return nil, nil
	case "start":
		return s.start(ctx)
	case "stop":
		s.workers.Stop()
		s.workers = utils.NewStoppableWorkers(s.cancelCtx)
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown command: %s", commandKey)
	}
}

func (s *vizF1viz) start(ctx context.Context) (map[string]interface{}, error) {
	if s.started.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("already started")
	}
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

	// Create buffered channel for locations
	s.locationChan = make(chan Location, locationChannelBuffer)

	// Create StoppableWorkers using cancelCtx
	s.workers = utils.NewStoppableWorkers(s.cancelCtx)

	// Create fetcher state
	state := &fetcherState{
		sessionKey:      sessionKey,
		lastFetchedTime: startTime,
		driverNumber:    44, // Default driver, could be made configurable
	}

	s.logger.Infof("Starting fetcher for session %d, starting from %s", sessionKey, startTime.Format(time.RFC3339))

	// Create fetcher worker with ticker (checks buffer and fetches every 1 second)
	s.workers.Add(func(ctx context.Context) {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		defer close(s.locationChan)

		for {
			select {
			case <-ctx.Done():
				s.logger.Info("Fetcher cancelled")
				return
			case <-ticker.C:
				s.fetcher(ctx, state)
			}
		}
	})

	// Create consumer worker
	s.workers.Add(func(ctx context.Context) {
		defer s.started.CompareAndSwap(true, false)
		s.consumer(ctx)
	})

	// Return immediately - workers run in background
	return map[string]interface{}{
		"status":  "started",
		"message": "Fetcher and consumer workers started",
	}, nil
}

// fetcherState holds state for the fetcher worker
type fetcherState struct {
	sessionKey      int
	lastFetchedTime time.Time
	driverNumber    int
}

// fetcher is the work function called by the ticker-based fetcher worker
func (s *vizF1viz) fetcher(ctx context.Context, state *fetcherState) {
	// Check buffer level
	bufferLevel := float64(len(s.locationChan)) / float64(cap(s.locationChan))
	if bufferLevel < bufferLowThreshold {
		// Fetch next window
		endTime := state.lastFetchedTime.Add(fetchWindowDuration)
		locations, err := s.fetchLocationData(ctx, state.sessionKey, state.driverNumber, state.lastFetchedTime, endTime)
		if err != nil {
			s.logger.Errorf("Failed to fetch location data: %v", err)
			// Continue - don't exit on error, just retry next tick
			return
		}

		if len(locations) == 0 {
			// No more data available - stop the workers
			s.logger.Info("No more location data available, closing channel and stopping workers")
			close(s.locationChan)
			s.workers.Stop()
			return
		}

		// Send locations to channel
		for _, loc := range locations {
			select {
			case <-ctx.Done():
				return
			case s.locationChan <- loc:
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

		s.logger.Debugf("Fetched %d locations, buffer level: %.2f%%", len(locations), bufferLevel*100)
	}
}

// consumer continuously consumes and renders location data
func (s *vizF1viz) consumer(ctx context.Context) {
	s.logger.Info("Consumer started, waiting for location data...")

	// Track locations for trail rendering
	locationHistory := make([]Location, 0, 10)
	trailLength := 5

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("Consumer cancelled")
			return
		case location, ok := <-s.locationChan:
			if !ok {
				// Channel closed, no more data
				s.logger.Info("Location channel closed, consumer stopping")
				return
			}

			// Add to history
			locationHistory = append(locationHistory, location)
			if len(locationHistory) > trailLength {
				locationHistory = locationHistory[len(locationHistory)-trailLength:]
			}

			// Render the location with trail
			if err := s.renderLocation(location, locationHistory); err != nil {
				s.logger.Errorf("Failed to render location: %v", err)
				// Continue rendering even if one fails
			}

			// Small delay to control render rate (similar to original 10ms)
			time.Sleep(10 * time.Millisecond)
		}
	}
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

// renderLocation renders a single location with trail
func (s *vizF1viz) renderLocation(location Location, history []Location) error {
	pc := pointcloud.NewBasicEmpty()

	// Render trail with fading intensity
	for i, loc := range history {
		// Calculate fade factor: most recent is 1.0, oldest fades to 0.0
		fadeFactor := float64(i) / float64(len(history)-1)
		if len(history) == 1 {
			fadeFactor = 1.0
		}

		// Color: red channel fades
		r := uint8(255 * fadeFactor)
		g := uint8(0)
		b := uint8(0)

		pc.Set(r3.Vector{
			X: float64(loc.X),
			Y: float64(loc.Y),
			Z: float64(loc.Z),
		}, pointcloud.NewColoredData(color.NRGBA{R: r, G: g, B: b, A: 255}))
	}

	// Note: This assumes you have a way to draw pointclouds
	// You may need to integrate with your visualization client here
	// For now, we'll just log that we're rendering
	s.logger.Debugf("Rendering location: (%d, %d, %d) with %d point trail",
		location.X, location.Y, location.Z, len(history))

	// TODO: Integrate with your visualization client
	// Example: vizClient.DrawPointCloud("movement", pc, nil)

	return nil
}

func (s *vizF1viz) Close(context.Context) error {
	s.cancelFunc()
	if s.workers != nil {
		s.workers.Stop()
	}
	return nil
}
