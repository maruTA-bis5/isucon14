package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/jmoiron/sqlx"
	"github.com/oklog/ulid/v2"
)

type chairPostChairsRequest struct {
	Name               string `json:"name"`
	Model              string `json:"model"`
	ChairRegisterToken string `json:"chair_register_token"`
}

type chairPostChairsResponse struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
}

func chairPostChairs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req := &chairPostChairsRequest{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Model == "" || req.ChairRegisterToken == "" {
		writeError(w, http.StatusBadRequest, errors.New("some of required fields(name, model, chair_register_token) are empty"))
		return
	}

	owner := &Owner{}
	if err := db.GetContext(ctx, owner, "SELECT * FROM owners WHERE chair_register_token = ?", req.ChairRegisterToken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, errors.New("invalid chair_register_token"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	chairID := ulid.Make().String()
	accessToken := secureRandomStr(32)

	_, err := db.ExecContext(
		ctx,
		"INSERT INTO chairs (id, owner_id, name, model, is_active, access_token) VALUES (?, ?, ?, ?, ?, ?)",
		chairID, owner.ID, req.Name, req.Model, false, accessToken,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Path:  "/",
		Name:  "chair_session",
		Value: accessToken,
	})

	writeJSON(w, http.StatusCreated, &chairPostChairsResponse{
		ID:      chairID,
		OwnerID: owner.ID,
	})
}

type postChairActivityRequest struct {
	IsActive bool `json:"is_active"`
}

func chairPostActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chair := ctx.Value("chair").(*Chair)

	req := &postChairActivityRequest{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	_, err := db.ExecContext(ctx, "UPDATE chairs SET is_active = ? WHERE id = ?", req.IsActive, chair.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type chairPostCoordinateResponse struct {
	RecordedAt int64 `json:"recorded_at"`
}

func chairPostCoordinate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req := &Coordinate{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	chair := ctx.Value("chair").(*Chair)

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	chairLocationID := ulid.Make().String()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO chair_locations (id, chair_id, latitude, longitude) VALUES (?, ?, ?, ?)`,
		chairLocationID, chair.ID, req.Latitude, req.Longitude,
	); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	location := &ChairLocation{}
	if err := tx.GetContext(ctx, location, `SELECT * FROM chair_locations WHERE id = ?`, chairLocationID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	ride := &Ride{}
	var rideStatusCode string
	if err := tx.GetContext(ctx, ride, `SELECT * FROM rides WHERE chair_id = ? ORDER BY updated_at DESC LIMIT 1`, chair.ID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	} else {
		status, err := getLatestRideStatus(ctx, tx, ride.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		rideStatusCode = status
		if status != "COMPLETED" && status != "CANCELED" {
			if req.Latitude == ride.PickupLatitude && req.Longitude == ride.PickupLongitude && status == "ENROUTE" {
				if _, err := tx.ExecContext(ctx, "INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)", ulid.Make().String(), ride.ID, "PICKUP"); err != nil {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
				rideStatusCode = "PICKUP"
			}

			if req.Latitude == ride.DestinationLatitude && req.Longitude == ride.DestinationLongitude && status == "CARRYING" {
				if _, err := tx.ExecContext(ctx, "INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)", ulid.Make().String(), ride.ID, "ARRIVED"); err != nil {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
			}
			rideStatusCode = "ARRIVED"
		}
	}

	sendLatestRideStatusForRide(ctx, tx, ride, rideStatusCode)

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, &chairPostCoordinateResponse{
		RecordedAt: location.CreatedAt.UnixMilli(),
	})
}

type simpleUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type chairGetNotificationResponse struct {
	Data         *chairGetNotificationResponseData `json:"data"`
	RetryAfterMs int                               `json:"retry_after_ms"`
}

type chairGetNotificationResponseData struct {
	RideID                string     `json:"ride_id"`
	User                  simpleUser `json:"user"`
	PickupCoordinate      Coordinate `json:"pickup_coordinate"`
	DestinationCoordinate Coordinate `json:"destination_coordinate"`
	Status                string     `json:"status"`
}

var sseServers = make(map[string]func(string))
var sseMutex sync.RWMutex

func chairGetNotification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	chair := ctx.Value("chair").(*Chair)

	// 	if sseServers[chair.ID] == nil {
	// 		sseServers[chair.ID] = &sse.Server{
	// 			Provider: &sse.Joe{
	// 				ReplayProvider: &sse.ValidReplayProvider{TTL: 30},
	// 			},
	// 			OnSession: func(s *sse.Session) (sse.Subscription, bool) {
	// 				return sendLatestRideStatus(chair, s)
	// 			},
	// 		}
	// 	}

	// }

	// func sseHandler(w http.ResponseWriter, r *http.Request) {
	// 必要なヘッダーを設定
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sseMutex.Lock()
	defer sseMutex.Unlock()
	// クライアントへのメッセージ送信関数
	sendMessage := func(data string) {
		fmt.Fprintf(w, "data: %s\n\n", data)
		// 即時に送信するためにフラッシュ
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}
	}
	sseServers[chair.ID] = sendMessage
	sendLatestRideStatusForChair(chair)

	// // 定期的にデータを送信
	// ticker := time.NewTicker(2 * time.Second)
	// defer ticker.Stop()

	// for {
	//     select {
	//     case <-ticker.C:
	//         sendMessage(fmt.Sprintf("Current time: %s", time.Now().Format(time.RFC3339)))
	//     case <-r.Context().Done():
	//         // クライアントが切断された場合
	//         fmt.Println("Client disconnected")
	//         return
	//     }
	// }
}

type aboutRide struct {
	userID  string `db:"user_id"`
	chairID string `db:"chair_id"`
}
type userToNotify struct {
	ID        string `db:"id"`
	Firstname string `db:"firstname"`
	Lastname  string `db:"lastname"`
}

func sendLatestRideStatusForRide(ctx context.Context, tx *sqlx.Tx, ride *Ride, status string) {
	user := &User{}
	if err := tx.GetContext(ctx, user, "SELECT * FROM users WHERE id = ?", ride.UserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		slog.Error("Error", err)
		return
	}

	sendLatestRideStatus(user, ride, status)
}
func sendLatestRideStatusForChair(chair *Chair) {
	ctx := context.Background()
	tx, err := db.Beginx()
	if err != nil {
		slog.Error("Error", err)
		return
	}
	defer tx.Rollback()

	ride := &Ride{}
	if err := tx.GetContext(ctx, ride, "SELECT * FROM rides WHERE chair_id = ? ORDER BY updated_at DESC LIMIT 1", chair.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			sseMutex.Lock()
			defer sseMutex.Unlock()
			sseServers[chair.ID]("data: {}\n")
			return
		}
		slog.Error("Error", err)
		return
	}

	status, err := getLatestRideStatus(ctx, tx, ride.ID)
	if err != nil {
		slog.Error("Error", err)
		return
	}

	sendLatestRideStatusForRide(ctx, tx, ride, status)
}
func sendLatestRideStatus(user *User, ride *Ride, status string) {
	if !ride.ChairID.Valid {
		return
	}
	// ctx := context.Background()
	tx, err := db.Beginx()
	if err != nil {
		// writeError(w, http.StatusInternalServerError, err)
		slog.Error("Error", err)
		return
	}
	defer tx.Rollback()
	// ride := &Ride{}
	// yetSentRideStatus := RideStatus{}
	// status := ""

	// if err := tx.GetContext(ctx, ride, `SELECT * FROM rides WHERE chair_id = ? ORDER BY updated_at DESC LIMIT 1`, chair.ID); err != nil {
	// if errors.Is(err, sql.ErrNoRows) {
	// writeJSON(w, http.StatusOK, &chairGetNotificationResponse{
	// 	RetryAfterMs: 30,
	// })
	// return
	// }
	// writeError(w, http.StatusInternalServerError, err)
	// slog.Error("Error", err)
	// return
	// }

	// if err := tx.GetContext(ctx, &yetSentRideStatus, `SELECT * FROM ride_statuses WHERE ride_id = ? AND chair_sent_at IS NULL ORDER BY created_at ASC LIMIT 1`, ride.ID); err != nil {
	// 	if errors.Is(err, sql.ErrNoRows) {
	// 		status, err = getLatestRideStatus(ctx, tx, ride.ID)
	// 		if err != nil {
	// 			// writeError(w, http.StatusInternalServerError, err)
	// 			slog.Error("Error", err)
	// 			return
	// 		}
	// 	} else {
	// 		// writeError(w, http.StatusInternalServerError, err)
	// 		slog.Error("Error", err)
	// 		return
	// 	}
	// } else {
	// 	status = yetSentRideStatus.Status
	// }

	// // user := &User{}
	// err = tx.GetContext(ctx, user, "SELECT * FROM users WHERE id = ? FOR SHARE", ride.UserID)
	// if err != nil {
	// 	// writeError(w, http.StatusInternalServerError, err)
	// 	slog.Error("Error", err)
	// 	return
	// }

	// if yetSentRideStatus.ID != "" {
	// 	_, err := tx.ExecContext(ctx, `UPDATE ride_statuses SET chair_sent_at = CURRENT_TIMESTAMP(6) WHERE id = ?`, yetSentRideStatus.ID)
	// 	if err != nil {
	// 		// writeError(w, http.StatusInternalServerError, err)
	// 		slog.Error("Error", err)
	// 		return
	// 	}
	// }

	// if err := tx.Commit(); err != nil {
	// 	// writeError(w, http.StatusInternalServerError, err)
	// 	slog.Error("Error", err)
	// 	return
	// }

	payload, err := json.Marshal(
		&chairGetNotificationResponseData{
			RideID: ride.ID,
			User: simpleUser{
				ID:   user.ID,
				Name: fmt.Sprintf("%s %s", user.Firstname, user.Lastname),
			},
			PickupCoordinate: Coordinate{
				Latitude:  ride.PickupLatitude,
				Longitude: ride.PickupLongitude,
			},
			DestinationCoordinate: Coordinate{
				Latitude:  ride.DestinationLatitude,
				Longitude: ride.DestinationLongitude,
			},
			Status: status,
		},
	)
	if err != nil {
		slog.Error("Error", err)
		return
	}
	sseMutex.RLock()
	defer sseMutex.RUnlock()
	sseServers[ride.ChairID.String](fmt.Sprintf("data: %s\n", string(payload)))
}

type postChairRidesRideIDStatusRequest struct {
	Status string `json:"status"`
}

func chairPostRideStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rideID := r.PathValue("ride_id")

	chair := ctx.Value("chair").(*Chair)

	req := &postChairRidesRideIDStatusRequest{}
	if err := bindJSON(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()

	ride := &Ride{}
	if err := tx.GetContext(ctx, ride, "SELECT * FROM rides WHERE id = ? FOR UPDATE", rideID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("ride not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if ride.ChairID.String != chair.ID {
		writeError(w, http.StatusBadRequest, errors.New("not assigned to this ride"))
		return
	}

	stsId := ulid.Make().String()
	switch req.Status {
	// Acknowledge the ride
	case "ENROUTE":
		if _, err := tx.ExecContext(ctx, "INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)", stsId, ride.ID, "ENROUTE"); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	// After Picking up user
	case "CARRYING":
		status, err := getLatestRideStatus(ctx, tx, ride.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if status != "PICKUP" {
			writeError(w, http.StatusBadRequest, errors.New("chair has not arrived yet"))
			return
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)", stsId, ride.ID, "CARRYING"); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	default:
		writeError(w, http.StatusBadRequest, errors.New("invalid status"))
	}

	sendLatestRideStatusForRide(ctx, tx, ride, req.Status)

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
