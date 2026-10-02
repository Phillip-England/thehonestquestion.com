package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const maxImageBytes = 20 << 20
const thumbnailWidth = 1200
const thumbnailHeight = 675

var imageNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}\.jpg$`)

func (s *server) uploadsDir() string { return filepath.Join(s.dataDir, "uploads") }

func (s *server) saveImage(source io.Reader, originalName string) (string, error) {
	dir := s.uploadsDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	input, err := os.CreateTemp(dir, "source-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(input.Name())
	size, err := io.Copy(input, io.LimitReader(source, maxImageBytes+1))
	if closeErr := input.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if size == 0 || size > maxImageBytes {
		return "", fmt.Errorf("image must be between 1 byte and 20 MB")
	}
	output, err := os.CreateTemp(dir, "thumbnail-*.jpg")
	if err != nil {
		return "", err
	}
	outputPath := output.Name()
	output.Close()
	defer os.Remove(outputPath)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	diagnostic, err := convertThumbnail(ctx, input.Name(), outputPath)
	if err != nil && isHEIF(originalName) && ctx.Err() == nil {
		intermediate, tempErr := os.CreateTemp(dir, "decoded-*.png")
		if tempErr != nil {
			return "", tempErr
		}
		intermediatePath := intermediate.Name()
		intermediate.Close()
		defer os.Remove(intermediatePath)
		decodeOutput, decodeErr := exec.CommandContext(ctx, "heif-convert", "--quiet", input.Name(), intermediatePath).CombinedOutput()
		if decodeErr == nil {
			diagnostic, err = convertThumbnail(ctx, intermediatePath, outputPath)
		} else {
			log.Printf("HEIC decode failed: %v: %.200s", decodeErr, decodeOutput)
		}
	}
	if err != nil {
		log.Printf("thumbnail conversion failed: %v: %.200s", err, diagnostic)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("image conversion timed out")
		}
		return "", fmt.Errorf("FFmpeg could not read this image")
	}
	info, err := os.Stat(outputPath)
	if err != nil || info.Size() == 0 {
		return "", fmt.Errorf("FFmpeg did not produce a thumbnail")
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	name := token + ".jpg"
	if err := os.Rename(outputPath, filepath.Join(dir, name)); err != nil {
		return "", err
	}
	return name, nil
}

func convertThumbnail(ctx context.Context, inputPath, outputPath string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", inputPath, "-map", "0:v:0", "-frames:v", "1", "-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,setsar=1", thumbnailWidth, thumbnailHeight, thumbnailWidth, thumbnailHeight), "-map_metadata", "-1", "-q:v", "3", "-f", "image2", outputPath)
	return cmd.CombinedOutput()
}

func isHEIF(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".heic", ".heif", ".avif":
		return true
	default:
		return false
	}
}

func (s *server) removeImage(name string) error {
	if name == "" {
		return nil
	}
	if !imageNamePattern.MatchString(name) {
		return fmt.Errorf("invalid stored image name")
	}
	err := os.Remove(filepath.Join(s.uploadsDir(), name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *server) serveImage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !imageNamePattern.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, filepath.Join(s.uploadsDir(), name))
}
