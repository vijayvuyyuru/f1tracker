package main

import (
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/golang/geo/r3"
	vizClient "github.com/viam-labs/motion-tools/client/client"
	"go.viam.com/rdk/pointcloud"
)

// Session represents a session from the OpenF1 API
type Session struct {
	SessionKey int    `json:"session_key"`
	DateStart  string `json:"date_start"`
	DateEnd    string `json:"date_end"`
	// Add other fields as needed
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

// TrackPoint represents a point on the reference track
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

// distance2D calculates the 2D Euclidean distance between two points (ignoring Z)
func distance2D(x1, y1, x2, y2 int) float64 {
	dx := float64(x2 - x1)
	dy := float64(y2 - y1)
	return math.Sqrt(dx*dx + dy*dy)
}

// generateReferenceTrack creates a reference track with 144 points from location data
// It extracts one complete lap and divides it into 144 evenly spaced segments
func generateReferenceTrack(locations []Location, startPoint TrackPoint) (*ReferenceTrack, error) {
	if len(locations) < 144 {
		return nil, fmt.Errorf("need at least 144 locations to generate reference track")
	}

	// Find the first complete lap by detecting when we return close to the start point
	startX := startPoint.X
	startY := startPoint.Y
	threshold := 1000.0 // Distance threshold to consider "close" to start

	var lapEndIdx int = len(locations) - 1
	for i := 100; i < len(locations); i++ {
		dist := distance2D(locations[i].X, locations[i].Y, startX, startY)
		if dist < threshold {
			lapEndIdx = i
			break
		}
	}

	// Use the first complete lap
	lapLocations := locations[0 : lapEndIdx+1]
	if len(lapLocations) < 144 {
		// If lap is shorter than 144 points, use all available points
		lapLocations = locations
	}

	// Calculate cumulative distances along the lap
	cumulativeDistances := make([]float64, len(lapLocations))
	cumulativeDistances[0] = 0.0
	var totalDist float64
	for i := 1; i < len(lapLocations); i++ {
		dist := distance2D(lapLocations[i-1].X, lapLocations[i-1].Y, lapLocations[i].X, lapLocations[i].Y)
		totalDist += dist
		cumulativeDistances[i] = totalDist
	}

	// Add distance from last point back to start point to close the loop
	distToStart := distance2D(lapLocations[len(lapLocations)-1].X, lapLocations[len(lapLocations)-1].Y, startX, startY)
	trackLength := totalDist + distToStart

	// Create 144 evenly spaced points along the track
	referencePoints := make([]TrackPoint, 144)

	for i := 0; i < 144; i++ {
		// Target distance for this index (0 to trackLength)
		// For i=143, we want to be at trackLength (back to start)
		targetDist := (float64(i) / 143.0) * trackLength

		// Find the two points that bracket this distance
		var point TrackPoint
		if i == 0 || i == 143 {
			// First and last points are always the start point to close the loop
			point = TrackPoint{X: startX, Y: startY, Z: startPoint.Z}
		} else {
			// Interpolate between two points
			found := false
			for j := 0; j < len(cumulativeDistances)-1; j++ {
				if cumulativeDistances[j] <= targetDist && cumulativeDistances[j+1] >= targetDist {
					// Interpolate between points j and j+1
					dist1 := cumulativeDistances[j]
					dist2 := cumulativeDistances[j+1]
					ratio := (targetDist - dist1) / (dist2 - dist1)

					point = TrackPoint{
						X: int(float64(lapLocations[j].X) + ratio*float64(lapLocations[j+1].X-lapLocations[j].X)),
						Y: int(float64(lapLocations[j].Y) + ratio*float64(lapLocations[j+1].Y-lapLocations[j].Y)),
						Z: int(float64(lapLocations[j].Z) + ratio*float64(lapLocations[j+1].Z-lapLocations[j].Z)),
					}
					found = true
					break
				}
			}
			// If target distance is beyond the last point, interpolate from last point to start
			if !found && targetDist > cumulativeDistances[len(cumulativeDistances)-1] {
				// Interpolate from last point to start point
				dist1 := cumulativeDistances[len(cumulativeDistances)-1]
				ratio := (targetDist - dist1) / distToStart

				lastPoint := lapLocations[len(lapLocations)-1]
				point = TrackPoint{
					X: int(float64(lastPoint.X) + ratio*float64(startX-lastPoint.X)),
					Y: int(float64(lastPoint.Y) + ratio*float64(startY-lastPoint.Y)),
					Z: int(float64(lastPoint.Z) + ratio*float64(startPoint.Z-lastPoint.Z)),
				}
			}
		}
		referencePoints[i] = point
	}

	return &ReferenceTrack{
		StartPoint: startPoint,
		Points:     referencePoints,
	}, nil
}

// mapLocationToIndex maps a single location to an index 0-143 using the reference track
// Returns the index of the closest point on the reference track
//
// Production usage:
//  1. Load reference track once at startup: track, err := loadReferenceTrack("reference_track.json")
//  2. For each location, call: index := mapLocationToIndex(location, track)
//  3. Index will be 0-143 representing position along the track
func mapLocationToIndex(location Location, track *ReferenceTrack) int {
	if len(track.Points) != 144 {
		return 0
	}

	minDist := math.MaxFloat64
	closestIdx := 0

	// Find the closest point on the reference track
	for i, refPoint := range track.Points {
		dist := distance2D(location.X, location.Y, refPoint.X, refPoint.Y)
		if dist < minDist {
			minDist = dist
			closestIdx = i
		}
	}

	return closestIdx
}

// loadReferenceTrack loads a reference track from a JSON file
func loadReferenceTrack(filename string) (*ReferenceTrack, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var track ReferenceTrack
	err = json.Unmarshal(data, &track)
	if err != nil {
		return nil, err
	}

	if len(track.Points) != 144 {
		return nil, fmt.Errorf("reference track must have exactly 144 points, got %d", len(track.Points))
	}

	return &track, nil
}

// saveReferenceTrack saves a reference track to a JSON file
func saveReferenceTrack(track *ReferenceTrack, filename string) error {
	data, err := json.MarshalIndent(track, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filename, data, 0644)
}

// mapLocationsToIndices maps locations to indices 0-143 based on cumulative distance along path
// Treats the first point as the start of a new lap
func mapLocationsToIndices(locations []Location) []int {
	if len(locations) == 0 {
		return []int{}
	}

	// First point is always index 0 (start of first lap)
	indices := make([]int, len(locations))
	indices[0] = 0

	// Calculate cumulative distances and detect lap boundaries
	cumulativeDistances := make([]float64, len(locations))
	cumulativeDistances[0] = 0.0

	// Track lap boundaries (indices where a new lap starts)
	lapStarts := []int{0} // First point is always a lap start

	// First point coordinates (reference point for all laps)
	firstPointX := locations[0].X
	firstPointY := locations[0].Y

	// Calculate cumulative distances
	var totalDistance float64
	for i := 1; i < len(locations); i++ {
		dist := distance2D(locations[i-1].X, locations[i-1].Y, locations[i].X, locations[i].Y)
		totalDistance += dist
		cumulativeDistances[i] = totalDistance
	}

	// Calculate threshold for detecting when we're close to the first point (lap boundary)
	// Use a percentage of the average segment length to determine threshold
	avgSegmentLength := totalDistance / float64(len(locations)-1)
	threshold := avgSegmentLength * 5.0 // 5x average segment length as threshold

	// Detect lap boundaries by checking distance to the first point
	// We need to ensure we've traveled a reasonable distance before checking for a new lap
	minPointsForLap := 50 // Minimum points before we can consider a new lap
	for i := minPointsForLap; i < len(locations); i++ {
		distToFirst := distance2D(locations[i].X, locations[i].Y, firstPointX, firstPointY)
		if distToFirst < threshold {
			// Check if we haven't already marked a lap start recently
			lastLapStart := lapStarts[len(lapStarts)-1]
			if i-lastLapStart > minPointsForLap {
				lapStarts = append(lapStarts, i)
			}
		}
	}

	// Map each point to its index within its lap
	for i := 0; i < len(locations); i++ {
		// Find which lap this point belongs to
		lapIndex := 0
		for j := len(lapStarts) - 1; j >= 0; j-- {
			if i >= lapStarts[j] {
				lapIndex = j
				break
			}
		}

		// Get the start and end indices for this lap
		lapStartIdx := lapStarts[lapIndex]
		var lapEndIdx int
		if lapIndex+1 < len(lapStarts) {
			lapEndIdx = lapStarts[lapIndex+1] - 1
		} else {
			lapEndIdx = len(locations) - 1
		}

		// Calculate cumulative distance within this lap
		lapStartDist := cumulativeDistances[lapStartIdx]
		lapEndDist := cumulativeDistances[lapEndIdx]
		pointDist := cumulativeDistances[i]

		// Normalize to 0-143
		if lapEndDist == lapStartDist {
			// Single point lap or no distance traveled
			indices[i] = 0
		} else {
			normalized := (pointDist - lapStartDist) / (lapEndDist - lapStartDist)
			index := int(math.Floor(normalized * 144))
			if index >= 144 {
				index = 143
			}
			indices[i] = index
		}
	}

	return indices
}

func main() {
	// Base URL
	baseURL := "https://api.openf1.org/v1/sessions"

	// Create URL with query parameters
	u, err := url.Parse(baseURL)
	if err != nil {
		fmt.Printf("Error parsing URL: %v\n", err)
		os.Exit(1)
	}

	// Add query parameters
	q := u.Query()
	q.Set("circuit_key", "9")     // Example value - adjust as needed
	q.Set("session_name", "Race") // Example value - adjust as needed
	q.Set("Year", "2023")
	u.RawQuery = q.Encode()

	// Make HTTP GET request
	resp, err := http.Get(u.String())
	if err != nil {
		fmt.Printf("Error making request: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Error reading response: %v\n", err)
		os.Exit(1)
	}

	// Parse sessions response
	var sessions []Session
	if err := json.Unmarshal(body, &sessions); err != nil {
		fmt.Printf("Error parsing sessions response: %v\n", err)
		fmt.Println("Response (raw):")
		fmt.Println(string(body))
		os.Exit(1)
	}

	// Pretty print sessions response
	prettyJSON, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		fmt.Printf("Error formatting JSON: %v\n", err)
		fmt.Println("Response (raw):")
		fmt.Println(string(body))
	} else {
		fmt.Println("Sessions Response:")
		fmt.Println(string(prettyJSON))
	}

	// Check if we have at least one session
	if len(sessions) == 0 {
		fmt.Println("No sessions found in response")
		os.Exit(1)
	}

	// Use the first session
	session := sessions[0]
	fmt.Printf("\nUsing session_key: %d\n", session.SessionKey)
	fmt.Printf("Session start: %s\n", session.DateStart)

	// Parse the start time
	startTime, err := time.Parse(time.RFC3339, session.DateStart)
	if err != nil {
		fmt.Printf("Error parsing start time: %v\n", err)
		os.Exit(1)
	}

	// Calculate first minute: start to start + 1 minute
	endTime := startTime.Add(5 * time.Minute)

	// Format times for the API query (OpenF1 expects format: 2006-01-02T15:04:05.000)
	// Convert to UTC and format without timezone
	startTimeUTC := startTime.UTC()
	endTimeUTC := endTime.UTC()
	startTimeStr := startTimeUTC.Format("2006-01-02T15:04:05.000")
	endTimeStr := endTimeUTC.Format("2006-01-02T15:04:05.000")

	fmt.Printf("Requesting location data from %s to %s\n\n", startTimeStr, endTimeStr)

	// Call location endpoint
	locationURL := "https://api.openf1.org/v1/location"
	locU, err := url.Parse(locationURL)
	if err != nil {
		fmt.Printf("Error parsing location URL: %v\n", err)
		os.Exit(1)
	}

	// Build query string manually to handle date> and date< parameters
	// OpenF1 API uses date> and date< as parameter names
	// URL encode the values
	startEncoded := url.QueryEscape(startTimeStr)
	endEncoded := url.QueryEscape(endTimeStr)

	// Construct the query string with special parameters
	queryString := fmt.Sprintf("session_key=%d&driver_number=44&date>%s&date<%s",
		session.SessionKey, startEncoded, endEncoded)
	locU.RawQuery = queryString

	fmt.Printf("Location API URL: %s\n\n", locU.String())

	// Make HTTP GET request to location endpoint
	locResp, err := http.Get(locU.String())
	if err != nil {
		fmt.Printf("Error making location request: %v\n", err)
		os.Exit(1)
	}
	defer locResp.Body.Close()

	// Read location response body
	locBody, err := io.ReadAll(locResp.Body)
	if err != nil {
		fmt.Printf("Error reading location response: %v\n", err)
		os.Exit(1)
	}

	// Parse and pretty print location response
	var locations []Location
	if err := json.Unmarshal(locBody, &locations); err != nil {
		fmt.Printf("Error parsing location response: %v\n", err)
		fmt.Println("Location Response (raw):")
		fmt.Println(string(locBody))
		os.Exit(1)
	}

	// Generate and save reference track for production use
	if len(locations) > 0 {
		startPoint := TrackPoint{
			X: locations[0].X,
			Y: locations[0].Y,
			Z: locations[0].Z,
		}

		// Generate reference track from location data
		referenceTrack, err := generateReferenceTrack(locations, startPoint)
		if err != nil {
			fmt.Printf("Error generating reference track: %v\n", err)
		} else {
			// Save reference track to JSON file
			err = saveReferenceTrack(referenceTrack, "reference_track.json")
			if err != nil {
				fmt.Printf("Error saving reference track: %v\n", err)
			} else {
				fmt.Printf("Reference track written to reference_track.json\n")
				fmt.Printf("Reference track contains %d points (indices 0-143)\n", len(referenceTrack.Points))
			}

			// Draw reference track as pointcloud
			drawReferenceTrack(referenceTrack)
		}
	}

	// Map locations to indices 0-143
	indices := mapLocationsToIndices(locations)
	fmt.Printf("Mapped %d locations to indices 0-143\n", len(indices))
	if len(indices) > 0 {
		fmt.Printf("First index: %d, Last index: %d\n", indices[0], indices[len(indices)-1])
	}

	drawMap(locations)

	for i := range locations {
		pc := pointcloud.NewBasicEmpty()

		// Draw trail of 5 points with fading intensity
		trailLength := 5
		startIdx := i - trailLength + 1
		if startIdx < 0 {
			startIdx = 0
		}

		// Get the base color intensity from the current point's index
		// idx := indices[i]
		baseR := float64(255) // Map 0-143 to 0-255 for red channel
		baseG := float64(0)
		baseB := float64(0)

		for j := startIdx; j <= i; j++ {
			// Calculate fade factor: most recent point (j==i) is 1.0, oldest fades to 0.0
			// Position in trail: 0 (oldest) to trailLength-1 (newest)
			positionInTrail := j - startIdx
			actualTrailLength := i - startIdx + 1
			// Fade from 1.0 (newest) to 0.0 (oldest) over the actual trail length
			fadeFactor := float64(positionInTrail) / float64(actualTrailLength-1)

			// Apply fade factor: newest point gets full intensity (R=255), older points fade to black
			r := uint8(baseR * fadeFactor)
			g := uint8(baseG * fadeFactor)
			b := uint8(baseB * fadeFactor)

			pc.Set(r3.Vector{
				X: float64(locations[j].X),
				Y: float64(locations[j].Y),
				Z: float64(locations[j].Z),
			}, pointcloud.NewColoredData(color.NRGBA{R: r, G: g, B: b, A: 255}))
		}

		vizClient.DrawPointCloud("movement", pc, nil)
		time.Sleep(10 * time.Millisecond)
	}

	// Production usage example: Load reference track and map arbitrary locations
	fmt.Println("\n=== Production Usage Example ===")
	referenceTrack, err := loadReferenceTrack("reference_track.json")
	if err != nil {
		fmt.Printf("Could not load reference track: %v\n", err)
		fmt.Println("(This is expected if reference_track.json doesn't exist yet)")
	} else {
		fmt.Println("Reference track loaded successfully")

		// Draw reference track as pointcloud
		drawReferenceTrack(referenceTrack)

		// Example: Map a few locations to indices
		if len(locations) > 0 {
			fmt.Println("\nMapping sample locations to indices:")
			for i := 0; i < len(locations) && i < 5; i++ {
				idx := mapLocationToIndex(locations[i], referenceTrack)
				fmt.Printf("Location %d: (%d, %d) -> Index %d\n", i, locations[i].X, locations[i].Y, idx)
			}
		}
	}
}

func drawMap(locations []Location) {
	pc := pointcloud.NewBasicEmpty()
	for _, location := range locations {
		pc.Set(r3.Vector{X: float64(location.X), Y: float64(location.Y), Z: float64(location.Z) - 100},
			pointcloud.NewColoredData(color.NRGBA{R: 0, G: 0, B: 0, A: 255}))
	}
	vizClient.DrawPointCloud("map", pc, nil)
}

// drawReferenceTrack reads the reference track and generates a pointcloud from it
func drawReferenceTrack(track *ReferenceTrack) {
	pc := pointcloud.NewBasicEmpty()

	// Draw all 144 points from the reference track
	for i, point := range track.Points {
		// Color based on index: gradient from blue (0) to red (143)
		r := uint8((i * 255) / 143)
		b := uint8(255 - (i * 255 / 143))

		pc.Set(r3.Vector{
			X: float64(point.X),
			Y: float64(point.Y),
			Z: float64(point.Z),
		}, pointcloud.NewColoredData(color.NRGBA{R: r, G: 0, B: b, A: 255}))
	}

	vizClient.DrawPointCloud("reference", pc, nil)
	fmt.Println("Reference track pointcloud drawn with title 'reference'")
}
