// idle-suspender suspends tailnet members who have been idle for longer than
// the configured threshold (default 30 days). Owners and admins are never
// auto-suspended. Pass --dry-run to preview changes without applying them.
//
// Usage:
//
//	TAILSCALE_API_KEY=tskey-api-... idle-suspender [flags]
//
// Flags:
//
//	--tailnet     tailnet domain or "-" for the key owner's tailnet (default "-")
//	--idle-days   number of days without activity before suspension (default 30)
//	--dry-run     print who would be suspended without making any changes
//	--slack-webhook  Slack incoming webhook URL for a post-run summary (optional)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tailscale/seat-manager/internal/tsapi"
)

// protectedRoles are never auto-suspended, even if idle.
var protectedRoles = map[string]bool{
	"owner":          true,
	"admin":          true,
	"it-admin":       true,
	"network-admin":  true,
	"billing-admin":  true,
	"device-admin":   true,
}

func main() {
	apiKey := flag.String("api-key", os.Getenv("TAILSCALE_API_KEY"), "Tailscale API key (or TAILSCALE_API_KEY env var)")
	tailnet := flag.String("tailnet", envOr("TAILSCALE_TAILNET", "-"), "Tailnet domain or '-'")
	idleDays := flag.Int("idle-days", 30, "Days without activity before suspending a user")
	dryRun := flag.Bool("dry-run", false, "Preview suspensions without applying them")
	slackWebhook := flag.String("slack-webhook", os.Getenv("SLACK_WEBHOOK_URL"), "Slack incoming webhook URL for summary (optional)")
	flag.Parse()

	if *apiKey == "" {
		log.Fatal("--api-key or TAILSCALE_API_KEY is required")
	}

	idleThreshold := time.Duration(*idleDays) * 24 * time.Hour

	ctx := context.Background()
	client := tsapi.New(*apiKey, *tailnet)

	users, err := client.ListUsers(ctx)
	if err != nil {
		log.Fatalf("listing users: %v", err)
	}

	var toSuspend []tsapi.User
	for _, u := range users {
		if protectedRoles[u.Role] {
			continue
		}
		if u.Status != "idle" {
			continue
		}
		// The API marks users idle at 28 days; enforce our own stricter threshold.
		if time.Since(u.LastSeen) < idleThreshold {
			continue
		}
		toSuspend = append(toSuspend, u)
	}

	if len(toSuspend) == 0 {
		fmt.Println("No idle users to suspend.")
		return
	}

	var suspended, failed []string
	for _, u := range toSuspend {
		if *dryRun {
			fmt.Printf("[dry-run] would suspend %s (%s), last seen %s\n",
				u.LoginName, u.DisplayName, u.LastSeen.Format(time.RFC3339))
			suspended = append(suspended, u.LoginName)
			continue
		}

		if err := client.SuspendUser(ctx, u.ID); err != nil {
			log.Printf("error suspending %s: %v", u.LoginName, err)
			failed = append(failed, u.LoginName)
			continue
		}
		log.Printf("suspended %s (%s), last seen %s", u.LoginName, u.DisplayName, u.LastSeen.Format(time.RFC3339))
		suspended = append(suspended, u.LoginName)
	}

	summary := buildSummary(*dryRun, *idleDays, suspended, failed)
	fmt.Println(summary)

	if *slackWebhook != "" {
		if err := postSlack(*slackWebhook, summary); err != nil {
			log.Printf("slack notification failed: %v", err)
		}
	}
}

func buildSummary(dryRun bool, idleDays int, suspended, failed []string) string {
	var b strings.Builder
	prefix := ""
	if dryRun {
		prefix = "[dry-run] "
	}
	fmt.Fprintf(&b, "%sSeat manager run complete (idle threshold: %d days)\n", prefix, idleDays)
	if len(suspended) > 0 {
		verb := "Suspended"
		if dryRun {
			verb = "Would suspend"
		}
		fmt.Fprintf(&b, "%s %d user(s): %s\n", verb, len(suspended), strings.Join(suspended, ", "))
	} else {
		fmt.Fprintf(&b, "No users to suspend.\n")
	}
	if len(failed) > 0 {
		fmt.Fprintf(&b, "Failed to suspend %d user(s): %s\n", len(failed), strings.Join(failed, ", "))
	}
	return b.String()
}

func postSlack(webhookURL, text string) error {
	payload, _ := json.Marshal(map[string]string{"text": text})
	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack webhook returned %d", resp.StatusCode)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
