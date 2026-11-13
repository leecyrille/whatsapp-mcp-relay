package relay

import (
	"context"
	"errors"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// An optional rule, set in relay.json as name_rule, that puts group chats whose name matches a pattern
// into one custom group, on the home PC, so it works with no app open. With no name_rule it never runs.
// Names are untrusted, but one only ever decides this: whether a chat that's in no group joins that group.
//
// The pattern is a Go regular expression, or "job-number" for the built-in one: 2 to 5 letters, optionally
// a dash and 1 or 2 more letters, then a dash and exactly four digits, anywhere in the name, such as
// WF-2601, ABC-A-2602 or "Pump Upgrade (XYZ-2602)".
//
// Each chat is decided once. A chat already in another group stays there, because a chat is only in one
// custom group, and if the user later takes a chat out of the rule's group, it isn't put back.

// JobNumberPattern is what "pattern": "job-number" means.
const JobNumberPattern = `(?:^|[^0-9A-Za-z])[A-Za-z]{2,5}(?:-[A-Za-z]{1,2})?-[0-9]{4}(?:[^0-9]|$)`

// NameRuleConfig is name_rule in relay.json. Group is the custom group's ID or name.
type NameRuleConfig struct {
	Group   string `json:"group"`
	Pattern string `json:"pattern"`
}

// NameRule is a checked NameRuleConfig.
type NameRule struct {
	group   string
	pattern *regexp.Regexp
}

func (c *NameRuleConfig) compile() (*NameRule, error) {
	if c == nil {
		return nil, nil
	}
	group, pattern := strings.TrimSpace(c.Group), strings.TrimSpace(c.Pattern)
	if group == "" || pattern == "" {
		return nil, errors.New("name_rule needs a group and a pattern")
	}
	if pattern == "job-number" {
		pattern = JobNumberPattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("name_rule pattern: %w", err)
	}
	return &NameRule{group: group, pattern: re}, nil
}

// NewNameRule checks a rule, for tests and callers outside the relay's config.
func NewNameRule(group, pattern string) (*NameRule, error) {
	return (&NameRuleConfig{Group: group, Pattern: pattern}).compile()
}

func (r *NameRule) matches(name string) bool {
	return r.pattern.MatchString(strings.TrimSpace(name))
}

const (
	ruleEvery = time.Minute
	ruleAdded = "added"
	ruleWasIn = "already-in:"
)

// NamedChat is one chat's address and name, as the bridge knows them.
type NamedChat struct {
	JID  string `json:"jid"`
	Name string `json:"name"`
}

func (g *Groups) ruleGroupLocked(idOrName string) *Group {
	if group := g.findLocked(idOrName); group != nil {
		return group
	}
	for i := range g.doc.Groups {
		if strings.EqualFold(g.doc.Groups[i].Name, idOrName) {
			return &g.doc.Groups[i]
		}
	}
	return nil
}

// ApplyNameRule decides any matching group chat it hasn't seen before, and says how many joined the rule's
// group. With no such group, it does nothing and decides nothing, so nothing is lost if one is added later.
func (g *Groups) ApplyNameRule(rule *NameRule, chats []NamedChat) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if rule == nil {
		return 0, nil
	}
	jobs := g.ruleGroupLocked(rule.group)
	if jobs == nil {
		return 0, nil
	}
	added, changed := 0, false
	for _, chat := range chats {
		if !strings.HasSuffix(chat.JID, "@g.us") || !jidRe.MatchString(chat.JID) || !rule.matches(chat.Name) {
			continue
		}
		if _, decided := g.doc.ProjectRule[chat.JID]; decided {
			continue
		}
		if g.doc.ProjectRule == nil {
			g.doc.ProjectRule = map[string]string{}
		}
		changed = true
		current := g.doc.Assign[chat.JID]
		if current != "" && g.findLocked(current) != nil {
			g.doc.ProjectRule[chat.JID] = ruleWasIn + current
			continue
		}
		g.doc.Assign[chat.JID] = jobs.ID
		g.doc.ProjectRule[chat.JID] = ruleAdded
		added++
	}
	if !changed {
		return 0, nil
	}
	return added, g.saveLocked()
}

// applyNameRule asks the bridge for group names and applies the rule. A failure just waits for the next
// round, since the bridge may be restarting.
func (s *Server) applyNameRule(ctx context.Context, rule *NameRule) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BridgeURL+"/api/app/group-names", nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	var body struct {
		Chats []NamedChat `json:"chats"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	added, err := s.groups.ApplyNameRule(rule, body.Chats)
	if err != nil {
		return err
	}
	if added > 0 {
		log.Printf("name rule: %d group chats joined %s", added, rule.group)
	}
	return nil
}

func (s *Server) ruleLoop(ctx context.Context, rule *NameRule) {
	for ctx.Err() == nil {
		_ = s.applyNameRule(ctx, rule)
		select {
		case <-ctx.Done():
			return
		case <-time.After(ruleEvery):
		}
	}
}
