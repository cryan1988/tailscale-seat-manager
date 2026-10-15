// slack-restore-bot is a Slack bot that lets suspended Tailscale users restore
// their own tailnet access via self-service. Identity is verified by matching
// the user's Slack workspace email (sourced from Okta via SCIM) against the
// suspended account's loginName in the Tailscale API.
//
// Required Slack app scopes:
//   - users:read.email  (look up the requester's email)
//   - chat:write        (send DM responses)
//
// Required Slack app features:
//   - Socket Mode enabled (set SLACK_APP_TOKEN to an xapp- token)
//   - Event Subscriptions: message.im
//   - Interactivity enabled
//
// Environment variables:
//
//	TAILSCALE_API_KEY   Tailscale API key
//	TAILSCALE_TAILNET   tailnet domain or "-" (default: "-")
//	SLACK_BOT_TOKEN     xoxb-... bot OAuth token
//	SLACK_APP_TOKEN     xapp-... app-level token for socket mode
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/tailscale/seat-manager/internal/tsapi"
)

const (
	actionRestoreAccess = "restore_access"
	actionDismiss       = "dismiss"
)

func main() {
	apiKey := mustEnv("TAILSCALE_API_KEY")
	tailnet := envOr("TAILSCALE_TAILNET", "-")
	botToken := mustEnv("SLACK_BOT_TOKEN")
	appToken := mustEnv("SLACK_APP_TOKEN")

	ts := tsapi.New(apiKey, tailnet)

	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	client := socketmode.New(api,
		socketmode.OptionLog(log.New(os.Stdout, "socketmode: ", log.LstdFlags)),
	)

	bot := &bot{api: api, ts: ts}

	go func() {
		for evt := range client.Events {
			switch evt.Type {
			case socketmode.EventTypeConnecting:
				log.Println("connecting to Slack...")
			case socketmode.EventTypeConnected:
				log.Println("connected to Slack via socket mode")

			case socketmode.EventTypeEventsAPI:
				client.Ack(*evt.Request)
				apiEvt, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				bot.handleEvent(apiEvt)

			case socketmode.EventTypeInteractive:
				client.Ack(*evt.Request)
				cb, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					continue
				}
				bot.handleInteraction(cb)

			case socketmode.EventTypeSlashCommand:
				client.Ack(*evt.Request)
				cmd, ok := evt.Data.(slack.SlashCommand)
				if !ok {
					continue
				}
				bot.handleSlashCommand(cmd)
			}
		}
	}()

	log.Fatal(client.Run())
}

type bot struct {
	api *slack.Client
	ts  *tsapi.Client
}

// handleEvent routes inbound Slack events. We only care about DMs.
func (b *bot) handleEvent(evt slackevents.EventsAPIEvent) {
	inner, ok := evt.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		return
	}
	// Only respond to DMs (channel type "im"), not bot messages or edits.
	if inner.ChannelType != "im" || inner.BotID != "" || inner.SubType != "" {
		return
	}
	b.checkAndRespond(inner.User, inner.Channel)
}

// handleSlashCommand handles /tailscale-restore from anywhere in Slack.
func (b *bot) handleSlashCommand(cmd slack.SlashCommand) {
	b.checkAndRespond(cmd.UserID, cmd.ChannelID)
}

// handleInteraction handles Block Kit button clicks.
func (b *bot) handleInteraction(cb slack.InteractionCallback) {
	if len(cb.ActionCallback.BlockActions) == 0 {
		return
	}
	action := cb.ActionCallback.BlockActions[0]

	switch action.ActionID {
	case actionRestoreAccess:
		b.doRestore(cb)
	case actionDismiss:
		b.updateMessage(cb.Channel.ID, cb.Message.Timestamp,
			":white_check_mark: Dismissed. Reach out to your IT admin if you need further help.")
	}
}

// checkAndRespond looks up the Slack user's Okta email, finds their Tailscale
// account, and posts an appropriate message.
func (b *bot) checkAndRespond(slackUserID, channelID string) {
	ctx := context.Background()

	email, err := b.slackEmail(slackUserID)
	if err != nil {
		b.postText(channelID, ":warning: Couldn't look up your email address. Please contact your IT admin.")
		log.Printf("slackEmail(%s): %v", slackUserID, err)
		return
	}

	tsUser, err := b.findUserByEmail(ctx, email)
	if err != nil {
		b.postText(channelID, ":warning: Error connecting to Tailscale API. Please try again later.")
		log.Printf("findUserByEmail(%s): %v", email, err)
		return
	}

	if tsUser == nil {
		b.postText(channelID, fmt.Sprintf(
			":x: No Tailscale account found for *%s*. "+
				"If this is unexpected, please contact your IT admin.", email))
		return
	}

	switch tsUser.Status {
	case "suspended":
		b.postSuspendedPrompt(channelID, email, tsUser.ID)
	case "active":
		b.postText(channelID, fmt.Sprintf(
			":white_check_mark: Your Tailscale account (*%s*) is *active*. "+
				"If you're having trouble connecting, try restarting the Tailscale client.", email))
	case "idle":
		b.postText(channelID, fmt.Sprintf(
			":large_yellow_circle: Your Tailscale account (*%s*) is active but has been *idle*. "+
				"No action needed — just reconnect with the Tailscale client.", email))
	default:
		b.postText(channelID, fmt.Sprintf(
			":grey_question: Your Tailscale account (*%s*) has status *%s*. "+
				"Please contact your IT admin for help.", email, tsUser.Status))
	}
}

// doRestore restores the suspended user identified by the button's value (userID:email).
func (b *bot) doRestore(cb slack.InteractionCallback) {
	ctx := context.Background()
	action := cb.ActionCallback.BlockActions[0]

	parts := strings.SplitN(action.Value, ":", 2)
	if len(parts) != 2 {
		b.updateMessage(cb.Channel.ID, cb.Message.Timestamp,
			":warning: Malformed restore request. Please contact your IT admin.")
		return
	}
	tsUserID, email := parts[0], parts[1]

	// Verify the Slack user who clicked is the account owner.
	claimedEmail, err := b.slackEmail(cb.User.ID)
	if err != nil || !strings.EqualFold(claimedEmail, email) {
		b.updateMessage(cb.Channel.ID, cb.Message.Timestamp,
			":no_entry: You can only restore your own account.")
		return
	}

	if err := b.ts.RestoreUser(ctx, tsUserID); err != nil {
		log.Printf("RestoreUser(%s): %v", tsUserID, err)
		b.updateMessage(cb.Channel.ID, cb.Message.Timestamp,
			":x: Failed to restore your account. Please contact your IT admin.")
		return
	}

	log.Printf("restored user %s (%s) via self-service Slack bot", email, tsUserID)
	b.updateMessage(cb.Channel.ID, cb.Message.Timestamp, fmt.Sprintf(
		":white_check_mark: Done! Your Tailscale account (*%s*) has been restored. "+
			"Open Tailscale and reconnect — you should be back on the tailnet.", email))
}

// postSuspendedPrompt sends the Block Kit card with a restore button.
func (b *bot) postSuspendedPrompt(channelID, email, tsUserID string) {
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType,
				fmt.Sprintf(":red_circle: Your Tailscale account (*%s*) is currently *suspended*.\n\n"+
					"This usually happens after 30 days of inactivity. "+
					"Click the button below to restore your access.", email),
				false, false),
			nil, nil),
		slack.NewActionBlock("",
			slack.NewButtonBlockElement(
				actionRestoreAccess,
				fmt.Sprintf("%s:%s", tsUserID, email), // payload: "userID:email"
				slack.NewTextBlockObject(slack.PlainTextType, "Restore my access", false, false),
			).WithStyle(slack.StylePrimary),
			slack.NewButtonBlockElement(
				actionDismiss,
				"dismiss",
				slack.NewTextBlockObject(slack.PlainTextType, "Dismiss", false, false),
			),
		),
	}

	if _, _, err := b.api.PostMessage(channelID, slack.MsgOptionBlocks(blocks...)); err != nil {
		log.Printf("PostMessage: %v", err)
	}
}

// postText posts a plain text message to a channel.
func (b *bot) postText(channelID, text string) {
	if _, _, err := b.api.PostMessage(channelID, slack.MsgOptionText(text, false)); err != nil {
		log.Printf("PostMessage: %v", err)
	}
}

// updateMessage replaces the original interactive message with a plain text result.
func (b *bot) updateMessage(channelID, ts, text string) {
	if _, _, _, err := b.api.UpdateMessage(channelID, ts, slack.MsgOptionText(text, false)); err != nil {
		log.Printf("UpdateMessage: %v", err)
	}
}

// slackEmail resolves a Slack user ID to their Okta-sourced email address.
func (b *bot) slackEmail(slackUserID string) (string, error) {
	info, err := b.api.GetUserInfo(slackUserID)
	if err != nil {
		return "", fmt.Errorf("GetUserInfo: %w", err)
	}
	if info.Profile.Email == "" {
		return "", fmt.Errorf("no email on Slack profile for %s", slackUserID)
	}
	return info.Profile.Email, nil
}

// findUserByEmail returns the Tailscale user whose loginName matches email (case-insensitive).
// Returns nil, nil when no match is found.
func (b *bot) findUserByEmail(ctx context.Context, email string) (*tsapi.User, error) {
	users, err := b.ts.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if strings.EqualFold(users[i].LoginName, email) {
			return &users[i], nil
		}
	}
	return nil, nil
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
