package domain

type AttentionQuestionKind string

const (
	AttentionQuestionKindQuestion      AttentionQuestionKind = "question"
	AttentionQuestionKindAuthorization AttentionQuestionKind = "authorization"
)

type AttentionQuestionPayload struct {
	SchemaVersion int                     `json:"schemaVersion" minimum:"1" maximum:"1"`
	Title         string                  `json:"title" minLength:"1" maxLength:"120"`
	Explanation   string                  `json:"explanation" minLength:"1" maxLength:"8000"`
	Questions     []AttentionPrompt       `json:"questions" minItems:"1" maxItems:"4"`
	Authorization *AttentionAuthorization `json:"authorization,omitempty"`
}

type AttentionPrompt struct {
	Key         string                    `json:"key" minLength:"1" maxLength:"64" pattern:"^[A-Za-z0-9][A-Za-z0-9_-]*$"`
	Header      string                    `json:"header" minLength:"1" maxLength:"24"`
	Question    string                    `json:"question" minLength:"1" maxLength:"2000"`
	Explanation *string                   `json:"explanation,omitempty" minLength:"1" maxLength:"4000"`
	MultiSelect bool                      `json:"multiSelect,omitempty"`
	Options     []AttentionQuestionOption `json:"options" minItems:"2" maxItems:"4"`
}

type AttentionQuestionOption struct {
	ID          string            `json:"id" minLength:"1" maxLength:"64" pattern:"^[A-Za-z0-9][A-Za-z0-9_-]*$"`
	Label       string            `json:"label" minLength:"1" maxLength:"80"`
	Description string            `json:"description" minLength:"1" maxLength:"1000"`
	Recommended bool              `json:"recommended,omitempty"`
	Preview     *AttentionPreview `json:"preview,omitempty"`
}

type AttentionPreview struct {
	Format  string `json:"format" enum:"text,markdown"`
	Content string `json:"content" minLength:"1" maxLength:"8192"`
}

type AttentionAuthorization struct {
	Action   string   `json:"action" minLength:"1" maxLength:"1000"`
	Scope    string   `json:"scope" minLength:"1" maxLength:"2000"`
	Limits   string   `json:"limits" minLength:"1" maxLength:"2000"`
	Evidence []string `json:"evidence" minItems:"1" maxItems:"4"`
}

type AttentionQuestionAnswer struct {
	QuestionKey       string   `json:"questionKey" minLength:"1" maxLength:"64"`
	SelectedOptionIDs []string `json:"selectedOptionIds" maxItems:"4"`
	OtherText         *string  `json:"otherText,omitempty" minLength:"1" maxLength:"4000"`
}

type AttentionAnswerSubmission struct {
	QuestionID      string                    `json:"questionId" minLength:"1" maxLength:"128"`
	ExpectedVersion int64                     `json:"expectedVersion" minimum:"1" maximum:"9007199254740991"`
	ClientAnswerID  string                    `json:"clientAnswerId" minLength:"1" maxLength:"128"`
	Answers         []AttentionQuestionAnswer `json:"answers" minItems:"1" maxItems:"4"`
}

const (
	MaxAttentionCreateBytes           = 128 * 1024
	MaxAttentionAnswerBytes           = 32 * 1024
	MaxAttentionQuestionVersion int64 = 1<<53 - 1
)

type AttentionValidationError struct {
	Field   string
	Message string
}

func (e *AttentionValidationError) Error() string { return e.Field + ": " + e.Message }
