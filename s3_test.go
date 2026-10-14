package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func testS3Client() *s3.Client {
	return s3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test-key", "test-secret", ""),
	})
}

func TestGeneratePresignedURL(t *testing.T) {
	signed, err := generatePresignedURL(testS3Client(), "test-bucket", "portrait/video with spaces.mp4", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "test-bucket.s3.us-east-1.amazonaws.com" || u.Path != "/portrait/video with spaces.mp4" || u.Query().Get("X-Amz-Expires") != "900" || u.Query().Get("X-Amz-Signature") == "" {
		t.Fatalf("unexpected presigned URL: %s", signed)
	}
	client := s3.NewFromConfig(aws.Config{
		Region: "us-east-1",
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{}, errors.New("credentials unavailable")
		}),
	})
	if signed, err := generatePresignedURL(client, "test-bucket", "video.mp4", time.Hour); err == nil || signed != "" {
		t.Fatalf("expected signing failure, got %q, %v", signed, err)
	}
}

func TestDBVideoToSignedVideo(t *testing.T) {
	cfg := apiConfig{s3Client: testS3Client()}
	for _, tc := range []struct {
		name     string
		location *string
		wantErr  bool
	}{
		{name: "no upload"},
		{name: "valid location", location: aws.String("test-bucket,portrait/video.mp4")},
		{name: "comma in key", location: aws.String("test-bucket,portrait/video,1.mp4")},
		{name: "missing delimiter", location: aws.String("invalid"), wantErr: true},
		{name: "empty bucket", location: aws.String(",video.mp4"), wantErr: true},
		{name: "empty key", location: aws.String("test-bucket,"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := database.Video{ID: uuid.New(), VideoURL: tc.location}
			video, err := cfg.dbVideoToSignedVideo(original)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if video.ID != original.ID || (tc.location == nil && video.VideoURL != nil) {
				t.Fatalf("unexpected video: %+v", video)
			}
			if tc.location != nil {
				u, err := url.Parse(*video.VideoURL)
				if err != nil || u.Query().Get("X-Amz-Signature") == "" || u.Path != "/"+(*tc.location)[len("test-bucket,"):] {
					t.Fatalf("unexpected signed URL: %v, %v", video.VideoURL, err)
				}
				if original.VideoURL == video.VideoURL {
					t.Fatal("signing reused the stored URL pointer")
				}
			}
		})
	}
}

func TestVideoRetrievalSigning(t *testing.T) {
	for _, list := range []bool{false, true} {
		for _, location := range []*string{nil, aws.String("test-bucket,portrait/video.mp4"), aws.String("invalid")} {
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
			cfg := apiConfig{db: db, jwtSecret: "test-secret", s3Client: testS3Client()}
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
			if location != nil && *location == "invalid" {
				if w.Code != http.StatusInternalServerError {
					t.Fatalf("expected signing error, got %d: %s", w.Code, w.Body.String())
				}
				continue
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
				t.Fatal("expected signed video URL")
			} else if u, err := url.Parse(*response.VideoURL); err != nil || u.Query().Get("X-Amz-Signature") == "" {
				t.Fatalf("unexpected URL: %s, %v", *response.VideoURL, err)
			}
			stored, err := db.GetVideo(video.ID)
			if err != nil || (location != nil && (stored.VideoURL == nil || *stored.VideoURL != *location)) {
				t.Fatalf("stored location changed: %+v, %v", stored, err)
			}
		}
	}
}
