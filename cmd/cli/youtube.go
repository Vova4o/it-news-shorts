package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/oauth2"

	"github.com/it-news-shorts/it-news-shorts/internal/youtube"
)

const youtubeUsage = `usage: cli youtube <command> [flags]

commands:
  auth       run the OAuth flow in a browser and store the token
  channel    show the connected channel
  upload     upload a video (private by default, needs --confirm-upload)
  status     show upload and processing status for a video id
  schedule   set a publish time (dry-run unless --confirm-schedule)
  uploads    list the uploads recorded in the local journal

environment:
  YOUTUBE_CLIENT_ID       OAuth Desktop client id
  YOUTUBE_CLIENT_SECRET   OAuth Desktop client secret
  YOUTUBE_TOKEN_FILE      token path (default: user config dir, outside the repo)
  YOUTUBE_JOURNAL_FILE    upload journal path
  YOUTUBE_CHANNEL_ID      expected channel id, verified after authorization
`

func runYouTube(args []string) {
	if len(args) == 0 {
		fmt.Print(youtubeUsage)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command, rest := args[0], args[1:]
	var err error
	switch command {
	case "auth":
		err = youtubeAuth(ctx, rest)
	case "channel":
		err = youtubeChannel(ctx, rest)
	case "upload":
		err = youtubeUpload(ctx, rest)
	case "status":
		err = youtubeStatus(ctx, rest)
	case "schedule":
		err = youtubeSchedule(ctx, rest)
	case "uploads":
		err = youtubeUploads(ctx, rest)
	case "help", "-h", "--help":
		fmt.Print(youtubeUsage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown youtube command %q\n\n", command)
		fmt.Print(youtubeUsage)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func youtubeTokenPath(getenv func(string) string) (string, error) {
	if path := strings.TrimSpace(getenv("YOUTUBE_TOKEN_FILE")); path != "" {
		return path, nil
	}
	return youtube.DefaultTokenPath()
}

func youtubeJournalPath(getenv func(string) string) (string, error) {
	if path := strings.TrimSpace(getenv("YOUTUBE_JOURNAL_FILE")); path != "" {
		return path, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(dir, "it-news-shorts", "uploads.json"), nil
}

func openJournal(getenv func(string) string) (*youtube.Journal, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	path, err := youtubeJournalPath(getenv)
	if err != nil {
		return nil, err
	}
	return youtube.OpenJournal(path)
}

// newYouTubeClient builds a client from the stored token and configured OAuth
// client credentials so expired access tokens refresh automatically.
func newYouTubeClient(ctx context.Context) (*youtube.Client, *youtube.Journal, error) {
	return newYouTubeClientWithEnv(ctx, os.Getenv, nil)
}

func newYouTubeClientWithEnv(ctx context.Context, getenv func(string) string, overrideConfig *youtube.OAuthConfig, opts ...youtube.Option) (*youtube.Client, *youtube.Journal, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var config youtube.OAuthConfig
	if overrideConfig != nil {
		config = *overrideConfig
	} else {
		clientID, clientSecret := youtube.EnvClientCredentials(getenv)
		var err error
		config, err = youtube.NewOAuthConfig(clientID, clientSecret)
		if err != nil {
			return nil, nil, err
		}
	}
	tokenPath, err := youtubeTokenPath(getenv)
	if err != nil {
		return nil, nil, err
	}
	store, err := youtube.NewTokenStore(tokenPath)
	if err != nil {
		return nil, nil, err
	}
	authorizer := &youtube.Authorizer{
		Config: config,
		Store:  store,
		Out:    os.Stdout,
	}
	source, err := authorizer.TokenSource(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w (run: cli youtube auth)", err)
	}
	journal, err := openJournal(getenv)
	if err != nil {
		return nil, nil, err
	}
	clientOpts := append([]youtube.Option{youtube.WithJournal(journal)}, opts...)
	return youtube.NewClient(source, clientOpts...), journal, nil
}

func youtubeAuth(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("youtube auth", flag.ContinueOnError)
	channelID := fs.String("channel-id", os.Getenv("YOUTUBE_CHANNEL_ID"), "expected channel id")
	timeout := fs.Duration("timeout", youtube.DefaultCallbackTimeout, "how long to wait for the browser redirect")
	if err := fs.Parse(args); err != nil {
		return err
	}

	clientID, clientSecret := youtube.EnvClientCredentials(os.Getenv)
	config, err := youtube.NewOAuthConfig(clientID, clientSecret)
	if err != nil {
		return err
	}
	tokenPath, err := youtubeTokenPath(os.Getenv)
	if err != nil {
		return err
	}
	store, err := youtube.NewTokenStore(tokenPath)
	if err != nil {
		return err
	}

	authorizer := &youtube.Authorizer{
		Config:          config,
		Store:           store,
		Out:             os.Stdout,
		CallbackTimeout: *timeout,
	}
	checkChannel := func(ctx context.Context, token *oauth2.Token) (youtube.Channel, error) {
		client := youtube.NewClient(oauth2.StaticTokenSource(token))
		return client.GetChannel(ctx)
	}

	channel, _, err := authorizer.Authorize(ctx, youtube.AuthorizeOptions{
		ExpectedChannelID: strings.TrimSpace(*channelID),
		CheckChannel:      checkChannel,
	})
	if err != nil {
		return err
	}
	if channel.ID != "" {
		fmt.Printf("Connected channel: %s (%s)\n", channel.Title, channel.ID)
	}
	return nil
}

func youtubeChannel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("youtube channel", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, _, err := newYouTubeClient(ctx)
	if err != nil {
		return err
	}
	channel, err := client.VerifyChannel(ctx, os.Getenv("YOUTUBE_CHANNEL_ID"))
	if err != nil {
		return err
	}
	fmt.Printf("Channel: %s\nID:      %s\n", channel.Title, channel.ID)
	return nil
}

func youtubeUpload(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("youtube upload", flag.ContinueOnError)
	file := fs.String("file", "", "path to the MP4 file")
	title := fs.String("title", "", "video title")
	description := fs.String("description", "", "video description")
	tags := fs.String("tags", "", "comma separated tags")
	category := fs.String("category", youtube.DefaultCategoryID, "YouTube category id")
	language := fs.String("language", "ru", "video language")
	madeForKids := fs.Bool("made-for-kids", false, "audience designation")
	synthetic := fs.Bool("synthetic", false, "declare altered or synthetic content")
	privacy := fs.String("privacy", string(youtube.PrivacyPrivate), "privacy status: private or unlisted")
	key := fs.String("idempotency-key", "", "idempotency key; defaults to a SHA-256 of file content and title")
	confirm := fs.Bool("confirm-upload", false, "perform the upload")
	if err := fs.Parse(args); err != nil {
		return err
	}

	visibility := youtube.PrivacyStatus(strings.ToLower(strings.TrimSpace(*privacy)))
	switch visibility {
	case youtube.PrivacyPrivate, youtube.PrivacyUnlisted:
	case youtube.PrivacyPublic:
		return errors.New("refusing to upload a public test video; use private or unlisted")
	default:
		return fmt.Errorf("unknown privacy status %q", *privacy)
	}

	input := youtube.UploadInput{
		FilePath:       *file,
		Title:          *title,
		Description:    *description,
		Tags:           splitTags(*tags),
		CategoryID:     *category,
		Language:       *language,
		MadeForKids:    *madeForKids,
		SyntheticMedia: *synthetic,
		Privacy:        visibility,
		IdempotencyKey: *key,
	}.Normalize()
	if input.IdempotencyKey == "" {
		derived, err := deriveIdempotencyKey(ctx, input.FilePath, input.Title)
		if err != nil {
			return err
		}
		input.IdempotencyKey = derived
	}
	if err := input.Validate(); err != nil {
		return err
	}

	if !*confirm {
		journalPath, err := youtubeJournalPath(os.Getenv)
		if err != nil {
			return err
		}
		fmt.Println("Dry run: no upload performed.")
		fmt.Printf("  file:            %s\n", input.FilePath)
		fmt.Printf("  title:           %s\n", input.Title)
		fmt.Printf("  privacy:         %s\n", input.Privacy)
		fmt.Printf("  category:        %s\n", input.CategoryID)
		fmt.Printf("  made for kids:   %t\n", input.MadeForKids)
		fmt.Printf("  synthetic:       %t\n", input.SyntheticMedia)
		fmt.Printf("  idempotency key: %s\n", input.IdempotencyKey)
		fmt.Printf("  journal:         %s\n", journalPath)
		fmt.Println("Re-run with --confirm-upload to upload.")
		return nil
	}

	client, _, err := newYouTubeClient(ctx)
	if err != nil {
		return err
	}
	if expected := strings.TrimSpace(os.Getenv("YOUTUBE_CHANNEL_ID")); expected != "" {
		if _, err := client.VerifyChannel(ctx, expected); err != nil {
			return err
		}
	}

	result, err := client.UploadVideo(ctx, input)
	if err != nil {
		if result.State != "" {
			fmt.Printf("State: %s\n", result.State)
		}
		return err
	}
	fmt.Printf("Uploaded: %s\n", result.URL)
	fmt.Printf("Video id: %s\n", result.VideoID)
	fmt.Printf("Privacy:  %s\n", result.PrivacyStatus)
	fmt.Printf("State:    %s\n", result.State)
	if result.Resumed {
		fmt.Println("Note: this result came from an existing record or session, not a new upload.")
	}
	return nil
}

func youtubeStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("youtube status", flag.ContinueOnError)
	id := fs.String("id", "", "YouTube video id")
	wait := fs.Bool("wait", false, "poll until processing finishes")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to poll with --wait")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*id) == "" {
		return errors.New("--id is required")
	}

	client, _, err := newYouTubeClient(ctx)
	if err != nil {
		return err
	}

	var status youtube.VideoStatus
	if *wait {
		status, err = client.WaitProcessing(ctx, *id, 5*time.Second, *timeout)
		if err != nil && status.VideoID == "" {
			return err
		}
	} else {
		status, err = client.GetVideo(ctx, *id)
		if err != nil {
			return err
		}
	}
	printVideoStatus(status)
	return err
}

func printVideoStatus(status youtube.VideoStatus) {
	fmt.Printf("Video id:      %s\n", status.VideoID)
	fmt.Printf("Title:         %s\n", status.Title)
	fmt.Printf("Privacy:       %s\n", status.PrivacyStatus)
	fmt.Printf("Upload status: %s\n", status.UploadStatus)
	fmt.Printf("Processing:    %s\n", status.ProcessingStatus)
	if !status.PublishAt.IsZero() {
		fmt.Printf("Publish at:    %s\n", status.PublishAt.UTC().Format(time.RFC3339))
	}
	if status.FailureReason != "" {
		fmt.Printf("Failure:       %s\n", status.FailureReason)
	}
	if status.RejectionReason != "" {
		fmt.Printf("Rejection:     %s\n", status.RejectionReason)
	}
}

func youtubeSchedule(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("youtube schedule", flag.ContinueOnError)
	id := fs.String("id", "", "YouTube video id")
	publishAt := fs.String("publish-at", "", "RFC3339 time, UTC or Europe/Moscow")
	confirm := fs.Bool("confirm-schedule", false, "actually send the update")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*id) == "" {
		return errors.New("--id is required")
	}
	when, err := parsePublishAt(*publishAt)
	if err != nil {
		return err
	}

	client, _, err := newYouTubeClient(ctx)
	if err != nil {
		return err
	}
	if *confirm {
		if expected := strings.TrimSpace(os.Getenv("YOUTUBE_CHANNEL_ID")); expected != "" {
			if _, err := client.VerifyChannel(ctx, expected); err != nil {
				return err
			}
		}
	}

	plan, err := client.SchedulePublication(ctx, *id, when, !*confirm)
	if err != nil {
		return err
	}
	fmt.Printf("Video id:   %s\n", plan.VideoID)
	fmt.Printf("Publish at: %s (UTC)\n", plan.PublishAt.Format(time.RFC3339))
	fmt.Printf("Privacy:    %s\n", plan.Privacy)
	fmt.Printf("Request:    %s\n", plan.Request)
	if plan.DryRun {
		fmt.Println("Dry run: no request sent. Re-run with --confirm-schedule to apply.")
		return nil
	}
	fmt.Println("Publication scheduled.")
	return nil
}

// parsePublishAt accepts RFC3339 and, when no zone is given, Europe/Moscow.
func parsePublishAt(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("--publish-at is required")
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), nil
	}
	moscow, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return time.Time{}, fmt.Errorf("load Europe/Moscow: %w", err)
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04"} {
		if parsed, err := time.ParseInLocation(layout, value, moscow); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse %q as RFC3339 or Europe/Moscow time", value)
}

func youtubeUploads(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("youtube uploads", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	journal, err := openJournal(os.Getenv)
	if err != nil {
		return err
	}
	entries, err := journal.List(ctx)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Printf("No uploads recorded in %s\n", journal.Path())
		return nil
	}
	fmt.Printf("Journal: %s\n\n", journal.Path())
	for _, entry := range entries {
		fmt.Printf("%s  %-11s %s\n", entry.UpdatedAt.Format(time.RFC3339), entry.State, entry.Key)
		if entry.VideoID != "" {
			fmt.Printf("    video:   %s\n", youtube.VideoURL(entry.VideoID))
		}
		if entry.Title != "" {
			fmt.Printf("    title:   %s\n", entry.Title)
		}
		if entry.Error != "" {
			fmt.Printf("    error:   %s\n", entry.Error)
		}
	}
	return nil
}

// deriveIdempotencyKey builds a stable key from the file content (SHA-256) and
// normalized title, so a repeated command maps to the same journal entry
// regardless of relative vs. absolute path or file modification time.
func deriveIdempotencyKey(ctx context.Context, path, title string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("--file is required")
	}
	preInfo, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat upload file: %w", err)
	}
	if !preInfo.Mode().IsRegular() {
		return "", fmt.Errorf("upload path %s is not a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("open upload file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat upload file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("upload path %s is not a regular file", path)
	}

	h := sha256.New()
	buf := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("hash upload file: %w", err)
		}
	}
	fileSum := hex.EncodeToString(h.Sum(nil))
	combined := sha256.Sum256([]byte(fileSum + "|" + strings.TrimSpace(title)))
	return "cli-" + hex.EncodeToString(combined[:12]), nil
}

func splitTags(raw string) []string {
	parts := strings.Split(raw, ",")
	tags := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			tags = append(tags, trimmed)
		}
	}
	return tags
}
