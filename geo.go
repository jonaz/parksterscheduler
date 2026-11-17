package main

import "math"

// Geofence is a circle around a point given in decimal degrees (WGS84).
type Geofence struct {
	Lat, Lon float64
	RadiusM  float64
}

// Contains reports whether the point is within the geofence radius.
func (g Geofence) Contains(lat, lon float64) bool {
	return distanceMeters(g.Lat, g.Lon, lat, lon) <= g.RadiusM
}

// distanceMeters returns the great-circle distance between two points (haversine).
func distanceMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusM = 6371000.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusM * math.Asin(math.Sqrt(a))
}
