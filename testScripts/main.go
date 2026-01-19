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

// distance2D calculates the 2D Euclidean distance between two points (ignoring Z)
func distance2D(x1, y1, x2, y2 int) float64 {
	dx := float64(x2 - x1)
	dy := float64(y2 - y1)
	return math.Sqrt(dx*dx + dy*dy)
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

	// Write start point for index calculations to JSON file
	if len(locations) > 0 {
		startPoint := Location{
			X:            locations[0].X,
			Y:            locations[0].Y,
			Z:            locations[0].Z,
			Date:         locations[0].Date,
			DriverNumber: locations[0].DriverNumber,
			MeetingKey:   locations[0].MeetingKey,
			SessionKey:   locations[0].SessionKey,
		}

		startPointJSON, err := json.MarshalIndent(startPoint, "", "  ")
		if err != nil {
			fmt.Printf("Error marshaling start point: %v\n", err)
		} else {
			err = os.WriteFile("start_point.json", startPointJSON, 0644)
			if err != nil {
				fmt.Printf("Error writing start point to file: %v\n", err)
			} else {
				fmt.Printf("Start point written to start_point.json\n")
			}
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

}

func drawMap(locations []Location) {
	pc := pointcloud.NewBasicEmpty()
	for _, location := range locations {
		pc.Set(r3.Vector{X: float64(location.X), Y: float64(location.Y), Z: float64(location.Z) - 100},
			pointcloud.NewColoredData(color.NRGBA{R: 0, G: 0, B: 0, A: 255}))
	}
	vizClient.DrawPointCloud("map", pc, nil)
}
