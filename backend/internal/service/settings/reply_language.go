package settings

import (
	"context"
	"errors"
	"fmt"
)

// ReplyLanguageSource reads the user preference at an agent's process boundary.
type ReplyLanguageSource interface {
	ReplyLanguage(context.Context) (string, error)
}

// ErrInvalidReplyLanguage identifies an unsupported preference at the API boundary.
var ErrInvalidReplyLanguage = errors.New("invalid reply language")

func replyLanguageName(language string) string {
	switch language {
	case "de":
		return "German"
	case "en":
		return "English"
	case "es":
		return "Spanish"
	case "fr":
		return "French"
	case "ja":
		return "Japanese"
	case "ko":
		return "Korean"
	case "pt-BR":
		return "Brazilian Portuguese"
	case "zh-CN":
		return "Simplified Chinese"
	case "zh-TW":
		return "Traditional Chinese"
	default:
		return ""
	}
}

// ReplyLanguagePrompt is shared by workers, orchestrators and native reviewers.
// Empty means disabled, preserving the existing prompt byte for byte.
func ReplyLanguagePrompt(language string) string {
	name := replyLanguageName(language)
	if name == "" {
		return ""
	}
	return fmt.Sprintf("## Agent Reply Language\n\nAlways write every reply to the user in %s (%s), including short confirmations, progress updates and long technical explanations. Write all ao report content in this language too. Keep code, paths, commands and identifiers in their original form.", name, language)
}

// ReplyLanguage reads fresh settings for each spawn, resume or restart.
func (s *Service) ReplyLanguage(ctx context.Context) (string, error) {
	snapshot, err := s.store.GetAppSettings(ctx)
	return snapshot.ReplyLanguage, err
}

// SetReplyLanguage validates and persists the language for future agent launches.
func (s *Service) SetReplyLanguage(ctx context.Context, language string) (Snapshot, error) {
	if language != "" && replyLanguageName(language) == "" {
		return Snapshot{}, fmt.Errorf("%w: %q", ErrInvalidReplyLanguage, language)
	}
	if err := s.store.SetReplyLanguage(ctx, language, s.now()); err != nil {
		return Snapshot{}, err
	}
	return s.Get(ctx)
}
