package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	baseURL    = "https://api.parkster.se/api/mobile/v2"
	apiVersion = "725"
	platform   = "android"
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

// CarSetter makes sure a given car is on the long-term parking permit.
type CarSetter interface {
	// EnsureCar returns the plate that was on the permit before, and whether it was changed.
	EnsureCar(plate string) (previous string, changed bool, err error)
}

type parksterClient struct {
	username, password string
	http               *http.Client
}

func newParksterClient(username, password string) *parksterClient {
	return &parksterClient{username: username, password: password, http: &http.Client{Timeout: 10 * time.Second}}
}

// EnsureCar sets the car on the first long-term parking and verifies the change.
func (p *parksterClient) EnsureCar(target string) (string, bool, error) {
	data, err := p.getUserData()
	if err != nil {
		return "", false, fmt.Errorf("get user data: %w", err)
	}
	if len(data.LongTermParkings) == 0 {
		return "", false, fmt.Errorf("no long-term parking found on the account")
	}
	// We assume we're managing the *first* long-term parking.
	parking := data.LongTermParkings[0]
	current := parking.Car.LicenseNbr
	if strings.EqualFold(current, target) {
		return current, false, nil
	}

	var carID int64 = -1
	for _, car := range data.Cars {
		if strings.EqualFold(car.LicenseNbr, target) {
			carID = car.ID
			break
		}
	}
	if carID == -1 {
		return current, false, fmt.Errorf("car %s not found in your Parkster account", target)
	}

	log.Printf("🔧 Changing car on parking %d from %s to %s (car ID %d)...", parking.ID, current, target, carID)
	if err := p.modifyParkingCar(parking.ID, carID); err != nil {
		return current, false, fmt.Errorf("change car: %w", err)
	}

	time.Sleep(2 * time.Second) // Give the API a moment to process.
	verified, err := p.getUserData()
	if err != nil {
		return current, false, fmt.Errorf("verify change: %w", err)
	}
	if len(verified.LongTermParkings) == 0 || !strings.EqualFold(verified.LongTermParkings[0].Car.LicenseNbr, target) {
		got := ""
		if len(verified.LongTermParkings) > 0 {
			got = verified.LongTermParkings[0].Car.LicenseNbr
		}
		return current, false, fmt.Errorf("verification failed: expected %s but found %s", target, got)
	}
	return current, true, nil
}

// getUserData fetches the main login object containing cars and parkings.
func (p *parksterClient) getUserData() (*LoginResponse, error) {
	loginURL := fmt.Sprintf("%s/people/login?version=%s&platform=%s", baseURL, apiVersion, platform)
	req, err := http.NewRequest("GET", loginURL, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(p.username, p.password)
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned non-200 status: %s", resp.Status)
	}

	var loginResponse LoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&loginResponse); err != nil {
		return nil, err
	}
	return &loginResponse, nil
}

// modifyParkingCar sends the POST request to change the car on a long-term parking permit.
func (p *parksterClient) modifyParkingCar(parkingID, carID int64) error {
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
	req.SetBasicAuth(p.username, p.password)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.http.Do(req)
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
