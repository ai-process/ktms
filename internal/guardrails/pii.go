package guardrails

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	emailRe = regexp.MustCompile(`\b[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}\b`)
	phoneRe = regexp.MustCompile(`(?:\+?\d{1,3}[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b`)
	ssnRe   = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	ipv4Re  = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\b`)
	// Broad pattern for candidate credit card sequences before Luhn validation
	cardCandidateRe = regexp.MustCompile(`\b(?:\d[ -]*?){13,19}\b`)
)

// IsValidLuhn checks whether a sequence of digits passes the Luhn checksum.
// Discards formatting spaces and dashes before computing sum.
func IsValidLuhn(number string) bool {
	digits := make([]int, 0, len(number))
	for _, r := range number {
		if unicode.IsDigit(r) {
			digits = append(digits, int(r-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	alternate := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if alternate {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alternate = !alternate
	}
	return sum%10 == 0
}

// PIISpan represents a range of text identified as sensitive information.
type PIISpan struct {
	Category ViolationCategory
	Start    int
	End      int
	Value    string
}

// DetectPII scans the input text for sensitive patterns.
func DetectPII(text string) []PIISpan {
	var spans []PIISpan

	// 1. Credit Cards with Luhn validation to suppress false positives
	for _, match := range cardCandidateRe.FindAllStringIndex(text, -1) {
		val := text[match[0]:match[1]]
		cleaned := strings.Map(func(r rune) rune {
			if unicode.IsDigit(r) {
				return r
			}
			return -1
		}, val)
		if len(cleaned) >= 13 && len(cleaned) <= 19 && IsValidLuhn(cleaned) {
			spans = append(spans, PIISpan{
				Category: CategoryCreditCard,
				Start:    match[0],
				End:      match[1],
				Value:    val,
			})
		}
	}

	// 2. Email addresses
	for _, match := range emailRe.FindAllStringIndex(text, -1) {
		spans = append(spans, PIISpan{
			Category: CategoryEmail,
			Start:    match[0],
			End:      match[1],
			Value:    text[match[0]:match[1]],
		})
	}

	// 3. Social Security Numbers
	for _, match := range ssnRe.FindAllStringIndex(text, -1) {
		spans = append(spans, PIISpan{
			Category: CategorySSN,
			Start:    match[0],
			End:      match[1],
			Value:    text[match[0]:match[1]],
		})
	}

	// 4. IP Addresses
	for _, match := range ipv4Re.FindAllStringIndex(text, -1) {
		spans = append(spans, PIISpan{
			Category: CategoryIPAddress,
			Start:    match[0],
			End:      match[1],
			Value:    text[match[0]:match[1]],
		})
	}

	// 5. Phone numbers
	for _, match := range phoneRe.FindAllStringIndex(text, -1) {
		val := text[match[0]:match[1]]
		// Avoid overlap with already matched credit card or IP
		if !overlaps(spans, match[0], match[1]) {
			spans = append(spans, PIISpan{
				Category: CategoryPhone,
				Start:    match[0],
				End:      match[1],
				Value:    val,
			})
		}
	}

	return spans
}

func overlaps(spans []PIISpan, start, end int) bool {
	for _, s := range spans {
		if (start >= s.Start && start < s.End) || (end > s.Start && end <= s.End) {
			return true
		}
	}
	return false
}
