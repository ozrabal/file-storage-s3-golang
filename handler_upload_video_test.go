package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func TestHandlerUploadVideo(t *testing.T) {
	installFakeFFprobe(t)
	for _, tc := range []struct {
		name         string
		mediaType    string
		field        string
		unauthorized bool
		missingAuth  bool
		invalidID    bool
		missingVideo bool
		s3Failure    bool
		probeOutput  string
		probeFailure bool
		prefix       string
		wantStatus   int
		wantUpload   bool
	}{
		{name: "success", mediaType: "video/mp4", field: "video", wantStatus: http.StatusOK, wantUpload: true},
		{name: "MIME parameters", mediaType: "video/mp4; codecs=avc1", field: "video", wantStatus: http.StatusOK, wantUpload: true},
		{name: "portrait", mediaType: "video/mp4", field: "video", probeOutput: `{"streams":[{"width":1080,"height":1920}]}`, prefix: "portrait", wantStatus: http.StatusOK, wantUpload: true},
		{name: "other", mediaType: "video/mp4", field: "video", probeOutput: `{"streams":[{"width":1000,"height":1000}]}`, prefix: "other", wantStatus: http.StatusOK, wantUpload: true},
		{name: "probe failure", mediaType: "video/mp4", field: "video", probeFailure: true, wantStatus: http.StatusInternalServerError},
		{name: "unsupported type", mediaType: "image/png", field: "video", wantStatus: http.StatusBadRequest},
		{name: "invalid type", mediaType: "invalid", field: "video", wantStatus: http.StatusBadRequest},
		{name: "missing file", mediaType: "video/mp4", field: "thumbnail", wantStatus: http.StatusBadRequest},
		{name: "wrong owner", unauthorized: true, wantStatus: http.StatusUnauthorized},
		{name: "missing auth", missingAuth: true, wantStatus: http.StatusUnauthorized},
		{name: "invalid ID", invalidID: true, wantStatus: http.StatusBadRequest},
		{name: "missing video", missingVideo: true, wantStatus: http.StatusNotFound},
		{name: "S3 failure", mediaType: "video/mp4", field: "video", s3Failure: true, wantStatus: http.StatusInternalServerError, wantUpload: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := tc.probeOutput
			if output == "" {
				output = `{"streams":[{"codec_type":"video","width":1920,"height":1080}]}`
			}
			t.Setenv("FFPROBE_TEST_OUTPUT", output)
			t.Setenv("FFPROBE_TEST_FAIL", "")
			if tc.probeFailure {
				t.Setenv("FFPROBE_TEST_FAIL", "1")
			}
			prefix := tc.prefix
			if prefix == "" {
				prefix = "landscape"
			}
			db, err := database.NewClient(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			ownerID := uuid.New()
			video, err := db.CreateVideo(database.CreateVideoParams{Title: "Test video", UserID: ownerID})
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte("test video contents")
			var uploadedKey string
			var uploads int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				uploads++
				if r.Method != http.MethodPut {
					t.Errorf("method = %s, want PUT", r.Method)
				}
				if r.Header.Get("Content-Type") != "video/mp4" {
					t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
				}
				uploadedKey = strings.TrimPrefix(r.URL.Path, "/test-bucket/")
				if !regexp.MustCompile(`^` + prefix + `/[0-9a-f]{64}\.mp4$`).MatchString(uploadedKey) {
					t.Errorf("unexpected object path: %s", r.URL.Path)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(body, payload) {
					t.Errorf("uploaded body = %q, error = %v", body, err)
				}
				if tc.s3Failure {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			cfg := apiConfig{
				db: db, jwtSecret: "test-secret", s3Bucket: "test-bucket", s3Region: "us-east-1",
				s3Client: s3.NewFromConfig(aws.Config{
					Region: "us-east-1", Credentials: aws.AnonymousCredentials{},
				}, func(o *s3.Options) {
					o.BaseEndpoint = aws.String(server.URL)
					o.UsePathStyle = true
				}),
			}

			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			if tc.field != "" {
				part, err := writer.CreatePart(textproto.MIMEHeader{
					"Content-Disposition": {`form-data; name="` + tc.field + `"; filename="video.mp4"`},
					"Content-Type":        {tc.mediaType},
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := part.Write(payload); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/api/video_upload/"+video.ID.String(), &body)
			request.SetPathValue("videoID", video.ID.String())
			if tc.invalidID {
				request.SetPathValue("videoID", "invalid")
			} else if tc.missingVideo {
				request.SetPathValue("videoID", uuid.NewString())
			}
			request.Header.Set("Content-Type", writer.FormDataContentType())
			if !tc.missingAuth {
				userID := ownerID
				if tc.unauthorized {
					userID = uuid.New()
				}
				token, err := auth.MakeJWT(userID, cfg.jwtSecret, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+token)
			}
			recorder := httptest.NewRecorder()
			cfg.handlerUploadVideo(recorder, request)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			wantUploads := 0
			if tc.wantUpload {
				wantUploads = 1
			}
			if uploads != wantUploads {
				t.Errorf("S3 uploads = %d, want %d", uploads, wantUploads)
			}
			stored, err := db.GetVideo(video.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantStatus == http.StatusOK {
				wantURL := "https://test-bucket.s3.us-east-1.amazonaws.com/" + uploadedKey
				if stored.VideoURL == nil || *stored.VideoURL != wantURL {
					t.Fatalf("stored video URL = %v, want %s", stored.VideoURL, wantURL)
				}
				var response database.Video
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.ID != video.ID || response.VideoURL == nil || *response.VideoURL != wantURL {
					t.Errorf("unexpected response: %+v", response)
				}
			} else if stored.VideoURL != nil {
				t.Errorf("failed request updated video URL: %s", *stored.VideoURL)
			}
		})
	}
}

func installFakeFFprobe(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
[ "$#" -eq 6 ] && [ "$1" = "-v" ] && [ "$2" = "error" ] && [ "$3" = "-print_format" ] && [ "$4" = "json" ] && [ "$5" = "-show_streams" ] && [ -s "$6" ] || exit 2
[ "$FFPROBE_TEST_FAIL" != "1" ] || exit 1
printf '%s' "$FFPROBE_TEST_OUTPUT"
`
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestGetVideoAspectRatio(t *testing.T) {
	installFakeFFprobe(t)
	filePath := filepath.Join(t.TempDir(), "video with spaces.mp4")
	if err := os.WriteFile(filePath, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		output  string
		fail    bool
		want    string
		wantErr bool
	}{
		{name: "landscape", output: `{"streams":[{"width":1920,"height":1080}]}`, want: "16:9"},
		{name: "portrait", output: `{"streams":[{"width":1080,"height":1920}]}`, want: "9:16"},
		{name: "rounded landscape", output: `{"streams":[{"width":1919,"height":1080}]}`, want: "16:9"},
		{name: "rounded portrait", output: `{"streams":[{"width":1080,"height":1919}]}`, want: "9:16"},
		{name: "square", output: `{"streams":[{"width":1080,"height":1080}]}`, want: "other"},
		{name: "audio first", output: `{"streams":[{"codec_type":"audio"},{"codec_type":"video","width":1920,"height":1080}]}`, want: "16:9"},
		{name: "invalid dimensions", output: `{"streams":[{"width":1920,"height":0}]}`, wantErr: true},
		{name: "no streams", output: `{"streams":[]}`, wantErr: true},
		{name: "invalid JSON", output: `invalid`, wantErr: true},
		{name: "command failure", fail: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FFPROBE_TEST_OUTPUT", tc.output)
			t.Setenv("FFPROBE_TEST_FAIL", "")
			if tc.fail {
				t.Setenv("FFPROBE_TEST_FAIL", "1")
			}
			got, err := getVideoAspectRatio(filePath)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("getVideoAspectRatio() = %q, %v; want %q, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
