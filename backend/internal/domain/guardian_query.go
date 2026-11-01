package domain

// GuardianFact is an internal bounded read row; only the explicit query DTO
// projection may leave the service. Sequence is an internal native key.
type GuardianFact struct {
	Kind     string
	Sequence string
	Request  *GuardianRequest
	Review   *GuardianReview
}
