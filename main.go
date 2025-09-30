package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	baseURL     = "https://api.parkster.se/api/mobile/v2"
	pushoverURL = "https://api.pushover.net/1/messages.json"
	apiVersion  = "725"
	platform    = "android"
	programName = "parksterScheduler"
)

// --- API Response Structures ---
// We only include the fields we need to interact with.

type Car struct {
	LicenseNbr string `json:"licenseNbr"`
	ID         int64  `json:"id"`
}

type LongTermParking struct {
	ID  int64 `json:"id"`
	Car Car   `json:"car"`
}

type LoginResponse struct {
	Cars             []Car             `json:"cars"`
	LongTermParkings []LongTermParking `json:"longTermParkings"`
}

// --- Schedule Structure ---

// Schedule maps a day of the week to a license plate number.
type Schedule map[string]string

// --- Main Application Logic ---

func main() {
	// 1. Define and parse command-line flags.
	username := flag.String("username", "", "Parkster account email")
	password := flag.String("password", "", "Parkster account password")
	schedulePath := flag.String("schedule", "schedule.json", "Path to the schedule JSON file")
	pushoverAPIKey := flag.String("pushover-api-key", "", "Pushover API Key/Token")
	pushoverUserKey := flag.String("pushover-user-key", "", "Pushover User Key")
	flag.Parse()

	if *username == "" || *password == "" {
		log.Fatal("❌ Username and password flags are required.")
	}

	// 2. Read and parse the schedule from the specified JSON file.
	schedule, err := loadSchedule(*schedulePath)
	if err != nil {
		errorMessage := fmt.Sprintf("Failed to load schedule file: %v", err)
		log.Fatal(errorMessage) // Program can't run without schedule, so we exit.
	}
	log.Println("✅ Schedule loaded successfully.")

	// 3. Run the main logic loop indefinitely.
	for {
		checkAndChangeCar(*username, *password, *pushoverAPIKey, *pushoverUserKey, schedule)

		// Calculate the duration until the next 7 AM.
		now := time.Now()
		loc, _ := time.LoadLocation("Europe/Stockholm")
		nextRun := time.Date(now.Year(), now.Month(), now.Day(), 7, 0, 0, 0, loc)
		if now.After(nextRun) {
			// If it's already past 7 AM today, schedule for tomorrow.
			nextRun = nextRun.Add(24 * time.Hour)
		}

		log.Printf("😴 Next check scheduled for %s. Sleeping for %v.", nextRun.Format("2006-01-02 15:04:05"), time.Until(nextRun))
		time.Sleep(time.Until(nextRun))
	}
}

// checkAndChangeCar contains the core logic for the daily operation.
func checkAndChangeCar(username, password, pushoverAPIKey, pushoverUserKey string, schedule Schedule) {
	log.Println("-------------------------------------------")
	log.Println("🚀 Starting daily parking check...")

	// 1. Determine which car should be active today.
	today := time.Now().Weekday().String()
	targetLicenseNbr, ok := schedule[today]
	if !ok {
		log.Printf("⚠️ No car scheduled for today (%s). Skipping.", today)
		return
	}
	log.Printf("🗓️ Today is %s. Scheduled car is %s.", today, targetLicenseNbr)

	// 2. Get current account data from Parkster API.
	data, err := getUserData(username, password)
	if err != nil {
		handleError(pushoverAPIKey, pushoverUserKey, "Failed to get user data: %v", err)
		return
	}

	// 3. Validate the data.
	if len(data.LongTermParkings) == 0 {
		log.Println("ℹ️ No long-term parkings found on this account. Nothing to do.")
		return
	}
	// We assume we're managing the *first* long-term parking.
	parking := data.LongTermParkings[0]
	currentLicenseNbr := parking.Car.LicenseNbr
	log.Printf("🔎 Current car on parking ID %d is %s.", parking.ID, currentLicenseNbr)

	// 4. Check if the car needs to be changed.
	if strings.EqualFold(currentLicenseNbr, targetLicenseNbr) {
		log.Printf("✅ Correct car (%s) is already assigned. No action needed.", currentLicenseNbr)
		return
	}
	log.Printf("⚠️ Car needs to be changed from %s to %s.", currentLicenseNbr, targetLicenseNbr)

	// 5. Find the ID of the target car from the user's list of cars.
	var targetCarID int64 = -1
	for _, car := range data.Cars {
		if strings.EqualFold(car.LicenseNbr, targetLicenseNbr) {
			targetCarID = car.ID
			break
		}
	}

	if targetCarID == -1 {
		handleError(pushoverAPIKey, pushoverUserKey, "Scheduled car %s not found in your Parkster account cars.", targetLicenseNbr)
		return
	}

	// 6. Perform the API call to change the car.
	log.Printf("🔧 Attempting to change car on parking %d to %s (Car ID: %d)...", parking.ID, targetLicenseNbr, targetCarID)
	err = modifyParkingCar(username, password, parking.ID, targetCarID)
	if err != nil {
		handleError(pushoverAPIKey, pushoverUserKey, "Failed to change car: %v", err)
		return
	}
	log.Println("👍 Car change request sent successfully.")

	// 7. Verification step: Fetch data again to confirm the change.
	log.Println("🔍 Verifying the change...")
	time.Sleep(2 * time.Second) // Give the API a moment to process.

	verifiedData, err := getUserData(username, password)
	if err != nil {
		handleError(pushoverAPIKey, pushoverUserKey, "Verification failed: Could not fetch user data again. Error: %v", err)
		return
	}

	verifiedParking := verifiedData.LongTermParkings[0]
	if strings.EqualFold(verifiedParking.Car.LicenseNbr, targetLicenseNbr) {
		log.Printf("✅ Verification successful! Car is now %s.", verifiedParking.Car.LicenseNbr)
	} else {
		handleError(pushoverAPIKey, pushoverUserKey, "VERIFICATION FAILED! Expected %s but found %s.", targetLicenseNbr, verifiedParking.Car.LicenseNbr)
	}
	log.Println("-------------------------------------------")
}

// --- Helper and API Functions ---

// handleError logs an error and sends a Pushover notification if keys are provided.
func handleError(apiKey, userKey, format string, a ...interface{}) {
	errorMessage := fmt.Sprintf(format, a...)
	log.Printf("❌ %s", errorMessage)
	sendPushoverNotification(apiKey, userKey, errorMessage)
}

// sendPushoverNotification sends a message using the Pushover API.
func sendPushoverNotification(apiKey, userKey, message string) {
	if apiKey == "" || userKey == "" {
		return // Do nothing if credentials are not provided.
	}

	// The title of the push notification
	title := fmt.Sprintf("Error in %s", programName)

	resp, err := http.PostForm(pushoverURL, url.Values{
		"token":   {apiKey},
		"user":    {userKey},
		"title":   {title},
		"message": {message},
	})

	if err != nil {
		log.Printf("🚨 Pushover notification failed to send: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("🚨 Pushover API returned non-200 status: %s. Body: %s", resp.Status, string(body))
	} else {
		log.Println("✉️ Pushover notification sent successfully.")
	}
}

// loadSchedule reads the schedule file from disk.
func loadSchedule(path string) (Schedule, error) {
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read file %s: %w", path, err)
	}

	var schedule Schedule
	err = json.Unmarshal(file, &schedule)
	if err != nil {
		return nil, fmt.Errorf("could not parse JSON from %s: %w", path, err)
	}
	return schedule, nil
}

// getUserData fetches the main login object containing cars and parkings.
func getUserData(username, password string) (*LoginResponse, error) {
	loginURL := fmt.Sprintf("%s/people/login?version=%s&platform=%s", baseURL, apiVersion, platform)
	req, err := http.NewRequest("GET", loginURL, nil)
	if err != nil {
		return nil, err
	}

	req.SetBasicAuth(username, password)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned non-200 status: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var loginResponse LoginResponse
	if err := json.Unmarshal(body, &loginResponse); err != nil {
		return nil, err
	}

	return &loginResponse, nil
}

// modifyParkingCar sends the POST request to change the car on a long-term parking permit.
func modifyParkingCar(username, password string, parkingID, carID int64) error {
	modifyURL := fmt.Sprintf(
		"%s/parkings/long-term/%d/modify-car?version=%s&platform=%s&platformVersion=36",
		baseURL, parkingID, apiVersion, platform,
	)

	formData := url.Values{}
	formData.Set("carId", fmt.Sprintf("%d", carID))

	req, err := http.NewRequest("POST", modifyURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return err
	}

	req.SetBasicAuth(username, password)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned non-200 status: %s. Body: %s", resp.Status, string(body))
	}

	return nil
}
