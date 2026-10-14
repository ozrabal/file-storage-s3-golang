package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func TestVideoRetrievalURLs(t *testing.T) {
	for _, list := range []bool{false, true} {
		for _, location := range []*string{nil, aws.String("https://d123example.cloudfront.net/portrait/video.mp4")} {
			db, err := database.NewClient(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			ownerID := uuid.New()
			video, err := db.CreateVideo(database.CreateVideoParams{Title: "Test", UserID: ownerID})
			if err != nil {
				t.Fatal(err)
			}
			video.VideoURL = location
			if err := db.UpdateVideo(video); err != nil {
				t.Fatal(err)
			}
			cfg := apiConfig{db: db, jwtSecret: "test-secret"}
			r := httptest.NewRequest(http.MethodGet, "/api/videos", nil)
			r.SetPathValue("videoID", video.ID.String())
			token, err := auth.MakeJWT(ownerID, cfg.jwtSecret, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			if list {
				cfg.handlerVideosRetrieve(w, r)
			} else {
				cfg.handlerVideoGet(w, r)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			var response database.Video
			if list {
				var videos []database.Video
				if err := json.Unmarshal(w.Body.Bytes(), &videos); err != nil || len(videos) != 1 {
					t.Fatalf("unexpected list: %s, %v", w.Body.String(), err)
				}
				response = videos[0]
			} else if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if location == nil {
				if response.VideoURL != nil {
					t.Fatal("expected null video URL")
				}
			} else if response.VideoURL == nil {
				t.Fatal("expected video URL")
			} else if *response.VideoURL != *location {
				t.Fatalf("video URL = %s, want %s", *response.VideoURL, *location)
			}
			stored, err := db.GetVideo(video.ID)
			if err != nil || (location != nil && (stored.VideoURL == nil || *stored.VideoURL != *location)) {
				t.Fatalf("stored location changed: %+v, %v", stored, err)
			}
		}
	}
}
