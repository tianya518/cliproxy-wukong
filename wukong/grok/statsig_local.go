package grok

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	statsigKeyword          = "obfiowerehiring"
	statsigEpoch            = int64(1682924400)
	statsigTrailer          = 3
	statsigAnimationTotal   = 4096
	statsigKeyByteCount     = 48
	statsigMaterialsTTL     = 15 * time.Minute
	statsigCurveGroupCount  = 4
	statsigCurveRowCount    = 16
	statsigCurveColorCount  = 6
	statsigCurveBezierCount = 4
)

type statsigCurveRow struct {
	Color  []int `json:"color"`
	Deg    int   `json:"deg"`
	Bezier []int `json:"bezier"`
}

type statsigPageMaterials struct {
	meta         string
	metaBytes    []byte
	frames       [][][]int
	animationKey string
	pagePath     string
	expiresAt    time.Time
	indices      statsigByteIndices
}

type statsigSignedID struct {
	Value        string
	PagePath     string
	MetaPrefix   string
	AnimationKey string
	HashInput    string
	Hash16       string
	Counter      int64
}

func signStatsigID(method, path string, materials statsigPageMaterials, now time.Time) (string, error) {
	signed, err := signStatsigIDWithTrace(method, path, materials, now)
	return signed.Value, err
}

func signStatsigIDWithTrace(method, path string, materials statsigPageMaterials, now time.Time) (statsigSignedID, error) {
	if len(materials.metaBytes) != statsigKeyByteCount || materials.animationKey == "" {
		return statsigSignedID{}, fmt.Errorf("Statsig 页面材料不完整")
	}
	counter := now.UTC().Unix() - statsigEpoch
	if counter < 0 {
		counter = 0
	}
	hashInput := strings.ToUpper(strings.TrimSpace(method)) + "!" + path + "!" + strconv.FormatInt(counter, 10) + statsigKeyword + materials.animationKey
	sum := sha256.Sum256([]byte(hashInput))
	block := make([]byte, 69)
	copy(block[:48], materials.metaBytes)
	block[48] = byte(counter)
	block[49] = byte(counter >> 8)
	block[50] = byte(counter >> 16)
	block[51] = byte(counter >> 24)
	copy(block[52:68], sum[:16])
	block[68] = statsigTrailer
	var xorKey [1]byte
	if _, err := rand.Read(xorKey[:]); err != nil {
		return statsigSignedID{}, err
	}
	raw := make([]byte, 70)
	raw[0] = xorKey[0]
	for index, value := range block {
		raw[index+1] = value ^ xorKey[0]
	}
	return statsigSignedID{
		Value:        base64.RawStdEncoding.EncodeToString(raw),
		PagePath:     materials.pagePath,
		MetaPrefix:   statsigMetaPrefix(materials.meta),
		AnimationKey: materials.animationKey,
		HashInput:    hashInput,
		Hash16:       fmt.Sprintf("%x", sum[:16]),
		Counter:      counter,
	}, nil
}

func statsigMetaPrefix(meta string) string {
	meta = strings.TrimSpace(meta)
	if len(meta) <= 12 {
		return meta
	}
	return meta[:12]
}

func buildStatsigMaterials(meta string, frames [][][]int, now time.Time) (statsigPageMaterials, error) {
	return buildStatsigMaterialsWith(meta, frames, defaultStatsigByteIndices(), now)
}

func buildStatsigMaterialsWith(meta string, frames [][][]int, indices statsigByteIndices, now time.Time) (statsigPageMaterials, error) {
	meta = strings.TrimSpace(meta)
	metaBytes, err := decodeStatsigMetaBytes(meta)
	if err != nil {
		return statsigPageMaterials{}, err
	}
	if err := validateStatsigFrames(frames); err != nil {
		return statsigPageMaterials{}, err
	}
	if indices.Source == "" {
		indices.Source = "default"
	}
	key, err := statsigAnimationKeyWith(metaBytes, frames, indices)
	if err != nil {
		return statsigPageMaterials{}, err
	}
	return statsigPageMaterials{
		meta:         meta,
		metaBytes:    metaBytes,
		frames:       frames,
		animationKey: key,
		expiresAt:    now.Add(statsigMaterialsTTL),
		indices:      indices,
	}, nil
}

func decodeStatsigMetaBytes(meta string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(meta)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(meta)
	}
	if err != nil || len(raw) != statsigKeyByteCount {
		return nil, fmt.Errorf("grok-site-verification 不是 48 字节")
	}
	return raw, nil
}

func validateStatsigFrames(frames [][][]int) error {
	if len(frames) != statsigCurveGroupCount {
		return fmt.Errorf("Botox 曲线组数无效")
	}
	for _, group := range frames {
		if len(group) != statsigCurveRowCount {
			return fmt.Errorf("Botox 曲线行数无效")
		}
		for _, row := range group {
			if len(row) != statsigCurveColorCount+1+statsigCurveBezierCount {
				return fmt.Errorf("Botox 曲线列数无效")
			}
		}
	}
	return nil
}

func statsigAnimationKey(keyBytes []byte, frames [][][]int) (string, error) {
	return statsigAnimationKeyWith(keyBytes, frames, defaultStatsigByteIndices())
}

func statsigAnimationKeyWith(keyBytes []byte, frames [][][]int, indices statsigByteIndices) (string, error) {
	if len(keyBytes) != statsigKeyByteCount {
		return "", fmt.Errorf("Statsig key 长度无效")
	}
	if indices.SVG < 0 || indices.SVG >= statsigKeyByteCount ||
		indices.Row < 0 || indices.Row >= statsigKeyByteCount ||
		indices.TimeA < 0 || indices.TimeA >= statsigKeyByteCount ||
		indices.TimeB < 0 || indices.TimeB >= statsigKeyByteCount ||
		indices.TimeC < 0 || indices.TimeC >= statsigKeyByteCount {
		return "", fmt.Errorf("Statsig 下标无效")
	}
	svg := int(keyBytes[indices.SVG]) % statsigCurveGroupCount
	row := int(keyBytes[indices.Row]) % statsigCurveRowCount
	frameTime := jsRound(float64((int(keyBytes[indices.TimeA])%16)*(int(keyBytes[indices.TimeB])%16)*(int(keyBytes[indices.TimeC])%16)) / 10)
	frameTime = frameTime * 10
	return animateStatsigRow(frames[svg][row], frameTime/statsigAnimationTotal), nil
}

func animateStatsigRow(row []int, targetTime float64) string {
	fromColor := []float64{float64(row[0]), float64(row[1]), float64(row[2]), 1}
	toColor := []float64{float64(row[3]), float64(row[4]), float64(row[5]), 1}
	toRotation := []float64{float64(int(math.Floor(solveStatsig(float64(row[6]), 60, 360, true))))}
	curves := make([]float64, 0, 4)
	for index, value := range row[7:] {
		minVal := 0.0
		if index%2 == 1 {
			minVal = -1
		}
		curves = append(curves, solveStatsig(float64(value), minVal, 1, false))
	}
	progress := cubicBezierValue(curves, targetTime)
	color := interpolateStatsig(fromColor, toColor, progress)
	for index := range color {
		if color[index] < 0 {
			color[index] = 0
		}
		if color[index] > 255 {
			color[index] = 255
		}
	}
	rotation := interpolateStatsig([]float64{0}, toRotation, progress)
	matrix := rotationMatrix(rotation[0])
	parts := make([]string, 0, 9)
	for _, value := range color[:3] {
		parts = append(parts, strconv.FormatInt(int64(jsRound(value)), 16))
	}
	for _, value := range matrix {
		rounded := math.Round(value*100) / 100
		if rounded < 0 {
			rounded = -rounded
		}
		hexValue := floatToHex(rounded)
		if strings.HasPrefix(hexValue, ".") {
			hexValue = "0" + hexValue
		}
		if hexValue == "" {
			hexValue = "0"
		}
		parts = append(parts, strings.ToLower(hexValue))
	}
	parts = append(parts, "0", "0")
	return strings.NewReplacer(".", "", "-", "").Replace(strings.Join(parts, ""))
}

func solveStatsig(value, minVal, maxVal float64, rounding bool) float64 {
	result := value*(maxVal-minVal)/255 + minVal
	if rounding {
		return math.Floor(result)
	}
	return math.Round(result*100) / 100
}

func interpolateStatsig(from, to []float64, factor float64) []float64 {
	out := make([]float64, len(from))
	for index := range from {
		out[index] = from[index]*(1-factor) + to[index]*factor
	}
	return out
}

func rotationMatrix(degrees float64) []float64 {
	radians := degrees * math.Pi / 180
	cos := math.Cos(radians)
	sin := math.Sin(radians)
	return []float64{cos, -sin, sin, cos}
}

func cubicBezierValue(curves []float64, time float64) float64 {
	if len(curves) < 4 {
		return time
	}
	startGradient := 0.0
	endGradient := 0.0
	start := 0.0
	end := 1.0
	if time <= 0 {
		if curves[0] > 0 {
			startGradient = curves[1] / curves[0]
		} else if curves[1] == 0 && curves[2] > 0 {
			startGradient = curves[3] / curves[2]
		}
		return startGradient * time
	}
	if time >= 1 {
		if curves[2] < 1 {
			endGradient = (curves[3] - 1) / (curves[2] - 1)
		} else if curves[2] == 1 && curves[0] < 1 {
			endGradient = (curves[1] - 1) / (curves[0] - 1)
		}
		return 1 + endGradient*(time-1)
	}
	for start < end {
		mid := (start + end) / 2
		estimated := cubicBezierCalculate(curves[0], curves[2], mid)
		if math.Abs(time-estimated) < 0.00001 {
			return cubicBezierCalculate(curves[1], curves[3], mid)
		}
		if estimated < time {
			start = mid
		} else {
			end = mid
		}
	}
	return cubicBezierCalculate(curves[1], curves[3], (start+end)/2)
}

func cubicBezierCalculate(a, b, m float64) float64 {
	return 3*a*(1-m)*(1-m)*m + 3*b*(1-m)*m*m + m*m*m
}

func jsRound(value float64) float64 {
	if value >= 0 {
		return math.Floor(value + 0.5)
	}
	return math.Ceil(value - 0.5)
}

func floatToHex(value float64) string {
	result := make([]byte, 0, 24)
	quotient := int(value)
	fraction := value - float64(quotient)
	x := value
	for quotient > 0 {
		quotient = int(x / 16)
		remainder := int(x - float64(quotient)*16)
		if remainder > 9 {
			result = append([]byte{byte('A' + remainder - 10)}, result...)
		} else {
			result = append([]byte{byte('0' + remainder)}, result...)
		}
		x = float64(quotient)
	}
	if fraction == 0 {
		return string(result)
	}
	result = append(result, '.')
	for fraction > 0 && len(result) < 24 {
		fraction *= 16
		integer := int(fraction)
		fraction -= float64(integer)
		if integer > 9 {
			result = append(result, byte('A'+integer-10))
		} else {
			result = append(result, byte('0'+integer))
		}
	}
	return string(result)
}

func loadStatsigFramesFile(path string) ([][][]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if frames, extractErr := extractStatsigCurves(data); extractErr == nil {
		return frames, nil
	}
	return extractStatsigCurves([]byte(`{"curves":` + string(data) + `}`))
}

func extractStatsigCurves(body []byte) ([][][]int, error) {
	raw := string(body)
	if frames, err := extractStatsigCurvesJSON(extractNextFlightText(raw)); err == nil {
		return frames, nil
	}
	text := strings.ReplaceAll(strings.ReplaceAll(raw, `\"`, `"`), `\n`, "\n")
	if frames, err := extractStatsigCurvesJSON(text); err == nil {
		return frames, nil
	}
	if frames, err := extractStatsigCurvesJSON(raw); err == nil {
		return frames, nil
	}
	if frames, err := extractStatsigCurvesSVG(text); err == nil {
		return frames, nil
	}
	if frames, err := extractStatsigCurvesSVG(raw); err == nil {
		return frames, nil
	}
	return nil, fmt.Errorf("页面缺少 Botox 曲线")
}

const nextFlightPushPrefix = `__next_f.push([1,"`

func extractNextFlightText(html string) string {
	if !strings.Contains(html, nextFlightPushPrefix) {
		return ""
	}
	var builder strings.Builder
	search := html
	for {
		index := strings.Index(search, nextFlightPushPrefix)
		if index < 0 {
			break
		}
		rest := search[index+len(nextFlightPushPrefix):]
		payload, consumed, ok := readJSStringBody(rest)
		if !ok {
			search = rest
			continue
		}
		builder.WriteString(payload)
		search = rest[consumed:]
	}
	return builder.String()
}

func readJSStringBody(source string) (string, int, bool) {
	var builder strings.Builder
	index := 0
	for index < len(source) {
		char := source[index]
		if char == '"' {
			return builder.String(), index + 1, true
		}
		if char != '\\' {
			builder.WriteByte(char)
			index++
			continue
		}
		if index+1 >= len(source) {
			return "", 0, false
		}
		esc := source[index+1]
		index += 2
		switch esc {
		case 'n':
			builder.WriteByte('\n')
		case 'r':
			builder.WriteByte('\r')
		case 't':
			builder.WriteByte('\t')
		case '"', '\\', '\'', '/':
			builder.WriteByte(esc)
		case 'u':
			if index+4 > len(source) {
				return "", 0, false
			}
			value, err := strconv.ParseInt(source[index:index+4], 16, 32)
			if err != nil {
				return "", 0, false
			}
			builder.WriteRune(rune(value))
			index += 4
		default:
			builder.WriteByte(esc)
		}
	}
	return "", 0, false
}

func extractStatsigCurvesJSON(text string) ([][][]int, error) {
	if text == "" {
		return nil, fmt.Errorf("页面缺少 Botox 曲线")
	}
	marker := `"curves":`
	search := text
	var lastErr error
	for {
		index := strings.Index(search, marker)
		if index < 0 {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("页面缺少 Botox 曲线")
		}
		search = search[index+len(marker):]
		frames, err := parseStatsigCurvesJSONArray(search)
		if err == nil {
			return frames, nil
		}
		lastErr = err
	}
}

func parseStatsigCurvesJSONArray(source string) ([][][]int, error) {
	payload, err := extractJSONArray(source)
	if err != nil {
		return nil, err
	}
	var rows [][]statsigCurveRow
	if err := json.Unmarshal(payload, &rows); err != nil {
		return nil, fmt.Errorf("解析 Botox 曲线: %w", err)
	}
	frames := make([][][]int, 0, len(rows))
	for _, group := range rows {
		converted := make([][]int, 0, len(group))
		for _, row := range group {
			if len(row.Color) != statsigCurveColorCount || len(row.Bezier) != statsigCurveBezierCount {
				return nil, fmt.Errorf("Botox 曲线字段不完整")
			}
			converted = append(converted, append(append(append([]int{}, row.Color...), row.Deg), row.Bezier...))
		}
		frames = append(frames, converted)
	}
	if err := validateStatsigFrames(frames); err != nil {
		return nil, err
	}
	return frames, nil
}

const statsigSVGPathPrefix = "M 10,30 C"

func extractStatsigCurvesSVG(text string) ([][][]int, error) {
	frames := make([][][]int, 0, statsigCurveGroupCount)
	seen := map[string]bool{}
	search := text
	for len(frames) < statsigCurveGroupCount {
		index := strings.Index(search, statsigSVGPathPrefix)
		if index < 0 {
			break
		}
		end := index
		for end < len(search) && search[end] != '"' && search[end] != '<' {
			end++
		}
		path := search[index:end]
		search = search[index+len(statsigSVGPathPrefix):]
		if seen[path] {
			continue
		}
		seen[path] = true
		rows := parseStatsigSVGPath(path)
		if len(rows) != statsigCurveRowCount {
			continue
		}
		frames = append(frames, rows)
	}
	if err := validateStatsigFrames(frames); err != nil {
		return nil, err
	}
	return frames, nil
}

func parseStatsigSVGPath(path string) [][]int {
	if len(path) < 9 {
		return nil
	}
	parts := strings.Split(path[9:], "C")
	rows := make([][]int, 0, len(parts))
	for _, part := range parts {
		fields := strings.FieldsFunc(part, func(r rune) bool {
			return r < '0' || r > '9'
		})
		if len(fields) == 0 {
			continue
		}
		row := make([]int, 0, len(fields))
		for _, field := range fields {
			value, err := strconv.Atoi(field)
			if err != nil {
				row = nil
				break
			}
			row = append(row, value)
		}
		if len(row) == statsigCurveColorCount+1+statsigCurveBezierCount {
			rows = append(rows, row)
		}
	}
	return rows
}

func extractJSONArray(source string) ([]byte, error) {
	start := strings.Index(source, "[")
	if start < 0 {
		return nil, fmt.Errorf("Botox 曲线不是 JSON 数组")
	}
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(source); {
		char, width := utf8.DecodeRuneInString(source[index:])
		if inString {
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				inString = false
			}
			index += width
			continue
		}
		switch char {
		case '"':
			inString = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return []byte(source[start : index+width]), nil
			}
		}
		index += width
	}
	return nil, fmt.Errorf("Botox 曲线 JSON 不完整")
}
