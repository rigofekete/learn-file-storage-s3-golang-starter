package main

import (
	"fmt"
	"net/http"
	"mime"
	"os"
	"io"
	"context"
	"bytes"
	"os/exec"
	"encoding/json"
	"errors"
	"path"
	"time"
	"strings"
 
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxMemory = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, maxMemory)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validat JWT", err)
		return
	}

	fmt.Println("uploading video", videoID, "by user", userID)

	video , err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting video from db", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized to update this video", err)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "error returning datat with form key", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", nil)
		return
	} 

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to create temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	if _, err = io.Copy(tempFile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error saving file", err)
		return
	}


	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error reseting temp file pointer", err)
		return
	}


	directory := ""
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting aspect ratio of uploaded video: ", err)
		return
	}

	switch aspectRatio {
	case "16:9":
		directory = "landscape/" 
	case "9:16":
		directory = "portrait/" 
	default:
		directory = "other/" 
	}



	key := getAssetPath(mediaType)
	key = path.Join(directory, key)

	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video for fast start", err)
		return
	}

	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error opening processed file", err)
		return
	}
	defer os.Remove(processedFile.Name())
	defer processedFile.Close()


	s3InObj := s3.PutObjectInput{
		Bucket: 	aws.String(cfg.s3Bucket),
		Key: 		aws.String(key),
		Body:		processedFile,
		ContentType: 	aws.String(mediaType),
	}
		
	_, err = cfg.s3Client.PutObject(
		context.Background(), 
		&s3InObj,  
	)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error puttig object into S3", err)
		return
	}

	signedURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, key)
	video.VideoURL = &signedURL

	
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error updating video in the database", err)
		return
	}

	video, err = cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate presigned URL: %v", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {

	cmd := exec.Command("ffprobe", "-v", 
		"error", "-print_format", 
		"json", "-show_streams", filePath,
	)
	
	var stdout bytes.Buffer
	cmd.Stdout = &stdout 

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("ffprobe error: %v", err)
	}

	var output struct {
		Streams []struct {
			Width 	int `json:"width"`
			Height 	int `json:"height"`
		} `json:"streams"`
	}
	
	err = json.Unmarshal(stdout.Bytes(), &output)
	if err != nil {
		return "", fmt.Errorf("could not parse ffprobe output: %v", err)
	}

	if len(output.Streams) == 0 {
		return "", errors.New("no video stream found")

	}

	width := output.Streams[0].Width  
	height := output.Streams[0].Height

	if width == 16*height/9 {
		return "16:9", nil
	} else if height == 16*width/9 {
		return "9:16", nil
	}
	return "other", nil
}


func processVideoForFastStart(filePath string) (string, error) {
	outputFile := filePath + ".processing"

	cmd := exec.Command("ffmpeg", 
		"-i", filePath, 
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4", outputFile,
	)

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("ffmpeg error: %v", err)
	}

	return outputFile, nil
}


func generatePresignedURL(s3client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	preSignClient := s3.NewPresignClient(s3client)

	params := s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:	aws.String(key),
	}

	req, err := preSignClient.PresignGetObject(context.Background(), &params, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("error getting presign object: %v", err)
	}

	return req.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}

	parts := strings.Split(*video.VideoURL, ",")
	if len(parts) != 2 {
		return video, nil 
	}

	bucket := parts[0]
	key := parts[1]

	preSignedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, 5*time.Minute)
	if err != nil {
		return video, nil 
	}

	video.VideoURL = &preSignedURL
	return video, nil
}
