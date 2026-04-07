package slack

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	"github.com/slack-go/slack"
)

type adminCmd int

const (
	cmdUnknown      adminCmd = iota
	cmdAllowUser             // allow user <@USERID>
	cmdDenyUser              // deny user <@USERID>
	cmdAllowChannel          // allow channel <#CHANID|name>
	cmdDenyChannel           // deny channel <#CHANID|name>
	cmdListAllowed           // list allowed
)

var (
	// Matches <@UXXXXXXXX> or <@UXXXXXXXX|displayname>
	userMentionRe = regexp.MustCompile(`<@([A-Z0-9]+)(?:\|[^>]*)?>`)
	// Matches <#CXXXXXXXX|channel-name>
	chanMentionRe = regexp.MustCompile(`<#([A-Z0-9]+)\|[^>]*>`)
	// Matches the leading bot mention at the start of the text
	leadingMentionRe = regexp.MustCompile(`^<@[A-Z0-9]+(?:\|[^>]*)?>\s*`)
)

// parseAdminCommand parses an app-mention text into an admin command and its
// target ID (Slack user ID or channel ID). Returns cmdUnknown if the text
// does not match any recognized admin command.
//
// Input is the raw ev.Text from an AppMentionEvent, which always begins with
// the bot's own mention, e.g. "<@UBOTID> allow user <@UTARGET>".
func parseAdminCommand(text string) (cmd adminCmd, targetID string) {
	// Strip the leading bot mention to get the command body
	body := leadingMentionRe.ReplaceAllString(text, "")
	body = strings.TrimSpace(body)
	lower := strings.ToLower(body)

	switch {
	case strings.HasPrefix(lower, "allow user"):
		m := userMentionRe.FindStringSubmatch(body)
		if len(m) >= 2 {
			return cmdAllowUser, m[1]
		}
	case strings.HasPrefix(lower, "deny user"), strings.HasPrefix(lower, "remove user"):
		m := userMentionRe.FindStringSubmatch(body)
		if len(m) >= 2 {
			return cmdDenyUser, m[1]
		}
	case strings.HasPrefix(lower, "allow channel"):
		m := chanMentionRe.FindStringSubmatch(body)
		if len(m) >= 2 {
			return cmdAllowChannel, m[1]
		}
	case strings.HasPrefix(lower, "deny channel"), strings.HasPrefix(lower, "remove channel"):
		m := chanMentionRe.FindStringSubmatch(body)
		if len(m) >= 2 {
			return cmdDenyChannel, m[1]
		}
	case strings.HasPrefix(lower, "list"):
		return cmdListAllowed, ""
	}
	return cmdUnknown, ""
}

// handleAdminCommand attempts to parse and execute an admin command from the
// raw AppMentionEvent text. Returns true if the text was recognized as an admin
// command (whether it succeeded or not), false if it should be forwarded to the agent.
func (a *SlackAdapter) handleAdminCommand(ctx context.Context, channelID, threadID, userID, text string) bool {
	cmd, targetID := parseAdminCommand(text)
	if cmd == cmdUnknown {
		return false
	}

	var reply string
	var err error

	switch cmd {
	case cmdAllowUser:
		err = a.allowList.addUser(ctx, targetID)
		if err != nil {
			reply = fmt.Sprintf(":x: Failed to add user <@%s>: %s", targetID, err)
		} else {
			reply = fmt.Sprintf(":white_check_mark: <@%s> has been granted access.", targetID)
			log.Printf("[Slack] Admin %s granted access to user %s", userID, targetID)
		}

	case cmdDenyUser:
		err = a.allowList.removeUser(ctx, targetID)
		if err != nil {
			reply = fmt.Sprintf(":x: Failed to remove user <@%s>: %s", targetID, err)
		} else {
			reply = fmt.Sprintf(":white_check_mark: <@%s> access has been revoked.", targetID)
			log.Printf("[Slack] Admin %s revoked access for user %s", userID, targetID)
		}

	case cmdAllowChannel:
		err = a.allowList.addChannel(ctx, targetID)
		if err != nil {
			reply = fmt.Sprintf(":x: Failed to add channel <#%s>: %s", targetID, err)
		} else {
			reply = fmt.Sprintf(":white_check_mark: <#%s> has been granted access.", targetID)
			log.Printf("[Slack] Admin %s granted access to channel %s", userID, targetID)
		}

	case cmdDenyChannel:
		err = a.allowList.removeChannel(ctx, targetID)
		if err != nil {
			reply = fmt.Sprintf(":x: Failed to remove channel <#%s>: %s", targetID, err)
		} else {
			reply = fmt.Sprintf(":white_check_mark: <#%s> access has been revoked.", targetID)
			log.Printf("[Slack] Admin %s revoked access for channel %s", userID, targetID)
		}

	case cmdListAllowed:
		reply = buildListReply(a.allowList)
	}

	_, _, postErr := a.client.PostMessageContext(ctx, channelID,
		slack.MsgOptionText(reply, false),
		slack.MsgOptionTS(threadID),
	)
	if postErr != nil {
		log.Printf("[Slack] Failed to post admin command reply: %v", postErr)
	}
	return true
}

func buildListReply(al *allowList) string {
	users := al.listUsers()
	channels := al.listChannels()
	sort.Strings(users)
	sort.Strings(channels)

	if len(users) == 0 && len(channels) == 0 {
		return ":information_source: No dynamic access grants. Only admins can use the bot."
	}

	var sb strings.Builder
	sb.WriteString("*Current access grants:*\n")
	if len(users) > 0 {
		sb.WriteString("*Users:*\n")
		for _, id := range users {
			fmt.Fprintf(&sb, "• <@%s>\n", id)
		}
	}
	if len(channels) > 0 {
		sb.WriteString("*Channels:*\n")
		for _, id := range channels {
			fmt.Fprintf(&sb, "• <#%s>\n", id)
		}
	}
	return sb.String()
}
