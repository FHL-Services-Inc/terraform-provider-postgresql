package postgresql

import (
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

const (
	queryConcatKeywordWhere = "WHERE"
	queryConcatKeywordAnd   = "AND"
	queryArrayKeywordAny    = "ANY"
	queryArrayKeywordAll    = "ALL"
	likePatternQuery        = "LIKE"
	notLikePatternQuery     = "NOT LIKE"
	regexPatternQuery       = "~"
)

// queryArgs collects the bind parameters of a query whose filters are built
// in pieces. User input (patterns, schema names, table types) is always passed
// as a parameter and never interpolated into the SQL text.
type queryArgs []any

// bind appends v and returns its placeholder ($1, $2, ...).
func (a *queryArgs) bind(v any) string {
	*a = append(*a, v)
	return fmt.Sprintf("$%d", len(*a))
}

// bindArray binds values as a text[] parameter for ANY/ALL comparisons.
func (a *queryArgs) bindArray(values []any) string {
	strs := make([]string, 0, len(values))
	for _, v := range values {
		strs = append(strs, v.(string))
	}
	return fmt.Sprintf("(%s::text[])", a.bind(pq.Array(strs)))
}

func applyPatternMatchingToQuery(patternMatchingTarget string, d *schema.ResourceData, args *queryArgs) []string {
	likeAnyPatterns := d.Get("like_any_patterns").([]any)
	likeAllPatterns := d.Get("like_all_patterns").([]any)
	notLikeAllPatterns := d.Get("not_like_all_patterns").([]any)
	regexPattern := d.Get("regex_pattern").(string)

	filters := []string{}
	if len(likeAnyPatterns) > 0 {
		filters = append(filters, generatePatternMatchingString(patternMatchingTarget, likePatternQuery, queryArrayKeywordAny+" "+args.bindArray(likeAnyPatterns)))
	}
	if len(likeAllPatterns) > 0 {
		filters = append(filters, generatePatternMatchingString(patternMatchingTarget, likePatternQuery, queryArrayKeywordAll+" "+args.bindArray(likeAllPatterns)))
	}
	if len(notLikeAllPatterns) > 0 {
		filters = append(filters, generatePatternMatchingString(patternMatchingTarget, notLikePatternQuery, queryArrayKeywordAll+" "+args.bindArray(notLikeAllPatterns)))
	}
	if regexPattern != "" {
		filters = append(filters, generatePatternMatchingString(patternMatchingTarget, regexPatternQuery, args.bind(regexPattern)))
	}

	return filters
}

func generatePatternMatchingString(patternMatchingTarget string, additionalQueryKeyword string, pattern string) string {
	patternMatchingFilter := fmt.Sprintf("%s %s %s", patternMatchingTarget, additionalQueryKeyword, pattern)

	return patternMatchingFilter
}

func applyTypeMatchingToQuery(objectKeyword string, objects []any, args *queryArgs) string {
	var typeFilter string
	if len(objects) > 0 {
		typeFilter = fmt.Sprintf("%s = %s %s", objectKeyword, queryArrayKeywordAny, args.bindArray(objects))
	}

	return typeFilter
}

// generatePatternArrayString renders a list of patterns for the data source
// IDs. It is not SQL and must not be used to build queries.
func generatePatternArrayString(patterns []any, queryArrayKeyword string) string {
	formattedPatterns := []string{}

	for _, pattern := range patterns {
		formattedPatterns = append(formattedPatterns, fmt.Sprintf("'%s'", pattern.(string)))
	}
	return fmt.Sprintf("%s (array[%s])", queryArrayKeyword, strings.Join(formattedPatterns, ","))
}

func finalizeQueryWithFilters(query string, queryConcatKeyword string, filters []string) string {
	if len(filters) > 0 {
		query = fmt.Sprintf("%s %s %s", query, queryConcatKeyword, strings.Join(filters, " AND "))
	}

	return query
}
