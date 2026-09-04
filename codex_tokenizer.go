package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const maxCodexJSONDepth = 10000

// codexJSONScanner is deliberately narrower than encoding/json. It validates
// JSON syntax, but only materializes fields that the Codex adapter consumes.
type codexJSONScanner struct {
	data []byte
	pos  int
}

func parseCodexLineTokenizer(line []byte) (codexLine, error) {
	scanner := codexJSONScanner{data: line}
	var row codexLine

	scanner.skipSpace()
	if scanner.consumeLiteral("null") {
		scanner.skipSpace()
		if scanner.pos != len(scanner.data) {
			return row, scanner.errorf("unexpected data after null")
		}
		return row, nil
	}
	if err := scanner.parseLineObject(&row); err != nil {
		return row, err
	}
	scanner.skipSpace()
	if scanner.pos != len(scanner.data) {
		return row, scanner.errorf("unexpected data after object")
	}
	return row, nil
}

func (s *codexJSONScanner) parseLineObject(row *codexLine) error {
	if err := s.expect('{'); err != nil {
		return err
	}

	var payload codexPayload
	payloadPresent := false
	var data, result, response codexResult
	dataPresent, resultPresent, responsePresent := false, false, false

	for {
		s.skipSpace()
		if s.consume('}') {
			break
		}
		key, err := s.readStringRaw()
		if err != nil {
			return err
		}
		if err := s.expect(':'); err != nil {
			return err
		}

		switch {
		case jsonKeyEquals(key, "timestamp"):
			row.Timestamp, err = s.readStringValue()
		case jsonKeyEquals(key, "created_at"):
			row.CreatedAt, err = s.readStringValue()
		case jsonKeyEquals(key, "createdAt"):
			row.CreatedAtCamel, err = s.readStringValue()
		case jsonKeyEquals(key, "type"):
			row.Type, err = s.readKnownType()
		case jsonKeyEquals(key, "model"):
			row.Model, err = s.readStringValue()
		case jsonKeyEquals(key, "model_name"):
			row.ModelName, err = s.readStringValue()
		case jsonKeyEquals(key, "payload"):
			if s.consumeLiteral("null") {
				payload = codexPayload{}
				payloadPresent = false
			} else {
				payloadPresent = true
				err = s.parsePayload(&payload, 0)
			}
		case jsonKeyEquals(key, "usage"):
			row.Usage, err = s.parseRawUsage(0)
		case jsonKeyEquals(key, "data"):
			if s.consumeLiteral("null") {
				data = codexResult{}
				dataPresent = false
			} else {
				dataPresent = true
				err = s.parseResult(&data, 0)
			}
		case jsonKeyEquals(key, "result"):
			if s.consumeLiteral("null") {
				result = codexResult{}
				resultPresent = false
			} else {
				resultPresent = true
				err = s.parseResult(&result, 0)
			}
		case jsonKeyEquals(key, "response"):
			if s.consumeLiteral("null") {
				response = codexResult{}
				responsePresent = false
			} else {
				responsePresent = true
				err = s.parseResult(&response, 0)
			}
		default:
			err = s.skipValue(0)
		}
		if err != nil {
			return err
		}

		s.skipSpace()
		if s.consume('}') {
			break
		}
		if !s.consume(',') {
			return s.errorf("expected comma or closing object")
		}
	}

	if payloadPresent && codexPayloadUseful(row.Type, payload) {
		row.Payload = &payload
	}
	if dataPresent && data.Usage != nil {
		row.Data = &data
	}
	if resultPresent && result.Usage != nil {
		row.Result = &result
	}
	if responsePresent && response.Usage != nil {
		row.Response = &response
	}
	return nil
}

func (s *codexJSONScanner) parsePayload(payload *codexPayload, depth int) error {
	if err := s.checkDepth(depth); err != nil {
		return err
	}
	if err := s.expect('{'); err != nil {
		return err
	}
	for {
		s.skipSpace()
		if s.consume('}') {
			return nil
		}
		key, err := s.readStringRaw()
		if err != nil {
			return err
		}
		if err := s.expect(':'); err != nil {
			return err
		}

		switch {
		case jsonKeyEquals(key, "type"):
			payload.Type, err = s.readKnownPayloadType()
		case jsonKeyEquals(key, "info"):
			payload.Info, err = s.parseInfo(depth + 1)
		case jsonKeyEquals(key, "model"):
			payload.Model, err = s.readStringValue()
		case jsonKeyEquals(key, "model_name"):
			payload.ModelName, err = s.readStringValue()
		case jsonKeyEquals(key, "id"):
			payload.ID, err = s.readStringValue()
		case jsonKeyEquals(key, "session_id"):
			payload.SessionID, err = s.readStringValue()
		case jsonKeyEquals(key, "cwd"):
			payload.CWD, err = s.readStringValue()
		case jsonKeyEquals(key, "trigger_turn"):
			payload.TriggerTurn, err = s.readBool()
		default:
			err = s.skipValue(depth + 1)
		}
		if err != nil {
			return err
		}

		s.skipSpace()
		if s.consume('}') {
			return nil
		}
		if !s.consume(',') {
			return s.errorf("expected comma or closing payload")
		}
	}
}

func (s *codexJSONScanner) parseInfo(depth int) (*codexInfo, error) {
	if s.consumeLiteral("null") {
		return nil, nil
	}
	if err := s.checkDepth(depth); err != nil {
		return nil, err
	}
	if err := s.expect('{'); err != nil {
		return nil, err
	}
	info := &codexInfo{}
	for {
		s.skipSpace()
		if s.consume('}') {
			return info, nil
		}
		key, err := s.readStringRaw()
		if err != nil {
			return nil, err
		}
		if err := s.expect(':'); err != nil {
			return nil, err
		}

		switch {
		case jsonKeyEquals(key, "last_token_usage"):
			info.LastTokenUsage, err = s.parseRawUsage(depth + 1)
		case jsonKeyEquals(key, "total_token_usage"):
			info.TotalTokenUsage, err = s.parseRawUsage(depth + 1)
		case jsonKeyEquals(key, "model"):
			info.Model, err = s.readStringValue()
		case jsonKeyEquals(key, "model_name"):
			info.ModelName, err = s.readStringValue()
		default:
			err = s.skipValue(depth + 1)
		}
		if err != nil {
			return nil, err
		}

		s.skipSpace()
		if s.consume('}') {
			return info, nil
		}
		if !s.consume(',') {
			return nil, s.errorf("expected comma or closing info")
		}
	}
}

func (s *codexJSONScanner) parseResult(result *codexResult, depth int) error {
	if err := s.checkDepth(depth); err != nil {
		return err
	}
	if err := s.expect('{'); err != nil {
		return err
	}
	for {
		s.skipSpace()
		if s.consume('}') {
			return nil
		}
		key, err := s.readStringRaw()
		if err != nil {
			return err
		}
		if err := s.expect(':'); err != nil {
			return err
		}

		switch {
		case jsonKeyEquals(key, "timestamp"):
			result.Timestamp, err = s.readStringValue()
		case jsonKeyEquals(key, "created_at"):
			result.CreatedAt, err = s.readStringValue()
		case jsonKeyEquals(key, "createdAt"):
			result.CreatedAtCamel, err = s.readStringValue()
		case jsonKeyEquals(key, "usage"):
			result.Usage, err = s.parseRawUsage(depth + 1)
		case jsonKeyEquals(key, "model"):
			result.Model, err = s.readStringValue()
		case jsonKeyEquals(key, "model_name"):
			result.ModelName, err = s.readStringValue()
		default:
			err = s.skipValue(depth + 1)
		}
		if err != nil {
			return err
		}

		s.skipSpace()
		if s.consume('}') {
			return nil
		}
		if !s.consume(',') {
			return s.errorf("expected comma or closing result")
		}
	}
}

func (s *codexJSONScanner) parseRawUsage(depth int) (*rawUsage, error) {
	if s.consumeLiteral("null") {
		return nil, nil
	}
	if err := s.checkDepth(depth); err != nil {
		return nil, err
	}
	if err := s.expect('{'); err != nil {
		return nil, err
	}
	usage := &rawUsage{}
	for {
		s.skipSpace()
		if s.consume('}') {
			return usage, nil
		}
		key, err := s.readStringRaw()
		if err != nil {
			return nil, err
		}
		if err := s.expect(':'); err != nil {
			return nil, err
		}

		field := rawUsageField(usage, key)
		if field != nil {
			var value flexUint64
			value, err = s.readFlexUint64()
			*field = value
		} else {
			err = s.skipValue(depth + 1)
		}
		if err != nil {
			return nil, err
		}

		s.skipSpace()
		if s.consume('}') {
			return usage, nil
		}
		if !s.consume(',') {
			return nil, s.errorf("expected comma or closing usage")
		}
	}
}

func rawUsageField(usage *rawUsage, key []byte) *flexUint64 {
	switch {
	case jsonKeyEquals(key, "input_tokens"):
		return &usage.Input
	case jsonKeyEquals(key, "prompt_tokens"):
		return &usage.Prompt
	case jsonKeyEquals(key, "input"):
		return &usage.InputAlt
	case jsonKeyEquals(key, "cached_input_tokens"):
		return &usage.CachedInput
	case jsonKeyEquals(key, "cache_read_input_tokens"):
		return &usage.CacheReadInput
	case jsonKeyEquals(key, "cached_tokens"):
		return &usage.CachedTokens
	case jsonKeyEquals(key, "cache_creation_input_tokens"):
		return &usage.CacheCreationInput
	case jsonKeyEquals(key, "cache_write_input_tokens"):
		return &usage.CacheWriteInput
	case jsonKeyEquals(key, "output_tokens"):
		return &usage.Output
	case jsonKeyEquals(key, "completion_tokens"):
		return &usage.Completion
	case jsonKeyEquals(key, "output"):
		return &usage.OutputAlt
	case jsonKeyEquals(key, "reasoning_output_tokens"):
		return &usage.ReasoningOutput
	case jsonKeyEquals(key, "reasoning_tokens"):
		return &usage.Reasoning
	case jsonKeyEquals(key, "total_tokens"):
		return &usage.Total
	default:
		return nil
	}
}

func (s *codexJSONScanner) readFlexUint64() (flexUint64, error) {
	s.skipSpace()
	if s.consumeLiteral("null") {
		return 0, nil
	}
	if s.peek() == '"' {
		value, err := s.readStringValue()
		if err != nil {
			return 0, err
		}
		return parseFlexUint64String(strings.TrimSpace(value))
	}
	number, err := s.readNumberRaw()
	if err != nil {
		return 0, err
	}
	return parseFlexUint64Number(number)
}

func parseFlexUint64String(value string) (flexUint64, error) {
	n, err := strconv.ParseUint(value, 10, 64)
	if err == nil {
		return flexUint64(n), nil
	}
	f, ferr := strconv.ParseFloat(value, 64)
	if ferr != nil || f < 0 || f > math.MaxUint64 {
		return 0, err
	}
	return flexUint64(uint64(f)), nil
}

func parseFlexUint64Number(number []byte) (flexUint64, error) {
	return parseFlexUint64String(string(number))
}

func (s *codexJSONScanner) readKnownType() (string, error) {
	raw, err := s.readStringRaw()
	if err != nil {
		return "", err
	}
	for _, value := range []string{
		"event_msg",
		"session_meta",
		"turn_context",
		"inter_agent_communication_metadata",
		"inter_agent_communication",
	} {
		if jsonKeyEquals(raw, value) {
			return value, nil
		}
	}
	return "", nil
}

func (s *codexJSONScanner) readKnownPayloadType() (string, error) {
	raw, err := s.readStringRaw()
	if err != nil {
		return "", err
	}
	for _, value := range []string{"token_count", "task_started", "turn_context"} {
		if jsonKeyEquals(raw, value) {
			return value, nil
		}
	}
	return "", nil
}

func codexPayloadUseful(rowType string, payload codexPayload) bool {
	switch rowType {
	case "session_meta", "turn_context", "event_msg", "inter_agent_communication_metadata", "inter_agent_communication":
		return true
	}
	switch payload.Type {
	case "token_count", "task_started", "turn_context":
		return true
	default:
		return false
	}
}

func (s *codexJSONScanner) skipValue(depth int) error {
	if err := s.checkDepth(depth); err != nil {
		return err
	}
	s.skipSpace()
	switch s.peek() {
	case '"':
		_, err := s.readStringRaw()
		return err
	case '{':
		return s.skipObject(depth + 1)
	case '[':
		return s.skipArray(depth + 1)
	case 't':
		if s.consumeLiteral("true") {
			return nil
		}
	case 'f':
		if s.consumeLiteral("false") {
			return nil
		}
	case 'n':
		if s.consumeLiteral("null") {
			return nil
		}
	default:
		_, err := s.readNumberRaw()
		return err
	}
	return s.errorf("invalid JSON value")
}

func (s *codexJSONScanner) skipObject(depth int) error {
	if err := s.checkDepth(depth); err != nil {
		return err
	}
	if err := s.expect('{'); err != nil {
		return err
	}
	for {
		s.skipSpace()
		if s.consume('}') {
			return nil
		}
		if _, err := s.readStringRaw(); err != nil {
			return err
		}
		if err := s.expect(':'); err != nil {
			return err
		}
		if err := s.skipValue(depth + 1); err != nil {
			return err
		}
		s.skipSpace()
		if s.consume('}') {
			return nil
		}
		if !s.consume(',') {
			return s.errorf("expected comma or closing object")
		}
	}
}

func (s *codexJSONScanner) skipArray(depth int) error {
	if err := s.checkDepth(depth); err != nil {
		return err
	}
	if err := s.expect('['); err != nil {
		return err
	}
	for {
		s.skipSpace()
		if s.consume(']') {
			return nil
		}
		if err := s.skipValue(depth + 1); err != nil {
			return err
		}
		s.skipSpace()
		if s.consume(']') {
			return nil
		}
		if !s.consume(',') {
			return s.errorf("expected comma or closing array")
		}
	}
}

func (s *codexJSONScanner) readStringValue() (string, error) {
	raw, err := s.readStringRaw()
	if err != nil {
		return "", err
	}
	value, err := strconv.Unquote(string(raw))
	if err != nil {
		return "", s.errorf("invalid JSON string: %v", err)
	}
	return value, nil
}

func (s *codexJSONScanner) readStringRaw() ([]byte, error) {
	s.skipSpace()
	if s.peek() != '"' {
		return nil, s.errorf("expected string")
	}
	start := s.pos
	s.pos++
	for s.pos < len(s.data) {
		c := s.data[s.pos]
		switch {
		case c == '"':
			s.pos++
			return s.data[start:s.pos], nil
		case c == '\\':
			s.pos++
			if s.pos >= len(s.data) {
				return nil, s.errorf("unterminated escape")
			}
			if s.data[s.pos] == 'u' {
				if s.pos+4 >= len(s.data) {
					return nil, s.errorf("short unicode escape")
				}
				for i := 1; i <= 4; i++ {
					if !isHexDigit(s.data[s.pos+i]) {
						return nil, s.errorf("invalid unicode escape")
					}
				}
				s.pos += 5
				continue
			}
			switch s.data[s.pos] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				s.pos++
			default:
				return nil, s.errorf("invalid escape")
			}
		case c < 0x20:
			return nil, s.errorf("unescaped control character in string")
		default:
			s.pos++
		}
	}
	return nil, s.errorf("unterminated string")
}

func (s *codexJSONScanner) readNumberRaw() ([]byte, error) {
	s.skipSpace()
	start := s.pos
	if s.consume('-') {
		if s.pos >= len(s.data) {
			return nil, s.errorf("invalid number")
		}
	}
	if s.consume('0') {
		// A leading zero must not be followed by another integer digit.
		if s.pos < len(s.data) && isDigit(s.data[s.pos]) {
			return nil, s.errorf("invalid number")
		}
	} else {
		if s.pos >= len(s.data) || s.data[s.pos] < '1' || s.data[s.pos] > '9' {
			return nil, s.errorf("invalid number")
		}
		for s.pos < len(s.data) && isDigit(s.data[s.pos]) {
			s.pos++
		}
	}
	if s.consume('.') {
		fractionStart := s.pos
		for s.pos < len(s.data) && isDigit(s.data[s.pos]) {
			s.pos++
		}
		if fractionStart == s.pos {
			return nil, s.errorf("invalid number fraction")
		}
	}
	if s.pos < len(s.data) && (s.data[s.pos] == 'e' || s.data[s.pos] == 'E') {
		s.pos++
		if s.pos < len(s.data) && (s.data[s.pos] == '+' || s.data[s.pos] == '-') {
			s.pos++
		}
		exponentStart := s.pos
		for s.pos < len(s.data) && isDigit(s.data[s.pos]) {
			s.pos++
		}
		if exponentStart == s.pos {
			return nil, s.errorf("invalid number exponent")
		}
	}
	return s.data[start:s.pos], nil
}

func (s *codexJSONScanner) readBool() (bool, error) {
	s.skipSpace()
	if s.consumeLiteral("true") {
		return true, nil
	}
	if s.consumeLiteral("false") {
		return false, nil
	}
	return false, s.errorf("expected boolean")
}

func (s *codexJSONScanner) expect(want byte) error {
	s.skipSpace()
	if !s.consume(want) {
		return s.errorf("expected %q", want)
	}
	return nil
}

func (s *codexJSONScanner) consume(want byte) bool {
	if s.pos < len(s.data) && s.data[s.pos] == want {
		s.pos++
		return true
	}
	return false
}

func (s *codexJSONScanner) consumeLiteral(literal string) bool {
	if len(s.data)-s.pos < len(literal) || string(s.data[s.pos:s.pos+len(literal)]) != literal {
		return false
	}
	s.pos += len(literal)
	return true
}

func (s *codexJSONScanner) peek() byte {
	if s.pos >= len(s.data) {
		return 0
	}
	return s.data[s.pos]
}

func (s *codexJSONScanner) skipSpace() {
	for s.pos < len(s.data) {
		switch s.data[s.pos] {
		case ' ', '\t', '\r', '\n':
			s.pos++
		default:
			return
		}
	}
}

func (s *codexJSONScanner) checkDepth(depth int) error {
	if depth > maxCodexJSONDepth {
		return s.errorf("JSON nesting exceeds %d", maxCodexJSONDepth)
	}
	return nil
}

func (s *codexJSONScanner) errorf(format string, args ...any) error {
	return fmt.Errorf("codex JSON at byte %d: %s", s.pos, fmt.Sprintf(format, args...))
}

func jsonKeyEquals(raw []byte, key string) bool {
	if len(raw) != len(key)+2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return false
	}
	for i := range key {
		if raw[i+1] != key[i] {
			return false
		}
	}
	return true
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}
