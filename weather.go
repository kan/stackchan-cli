package main

// Weather for say: {weather} (today) and {weather_tomorrow} read the Japan
// Meteorological Agency's forecast JSON (the data jma.go.jp's own pages
// use; Japanese weather text, chance of rain, high and low), and today adds
// the current temperature from Open-Meteo. The area is the user's, so it
// lives in ~/.stackchan/weather.txt, not in the repository.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

func weatherConfigPath() string { return filepath.Join(stackchanDir(), "weather.txt") }

// weatherConfig is ~/.stackchan/weather.txt: "key = value" lines, # comments.
type weatherConfig struct {
	Name      string // spoken place name
	Office    string // JMA forecast office (forecast/<office>.json), e.g. 130000
	Area      string // JMA primary subdivision for weather and rain, e.g. 130010
	Station   string // JMA temperature point in the same JSON, e.g. 44132
	Latitude  string // for Open-Meteo's current temperature (optional)
	Longitude string
}

const weatherConfigHelp = `~/.stackchan/weather.txt の例（東京）:
  name = 東京
  office = 130000
  area = 130010
  station = 44132
  latitude = 35.69
  longitude = 139.69`

func parseWeatherConfig(text string) (weatherConfig, error) {
	var c weatherConfig
	fields := map[string]*string{
		"name": &c.Name, "office": &c.Office, "area": &c.Area, "station": &c.Station,
		"latitude": &c.Latitude, "longitude": &c.Longitude,
	}
	err := parseKeyValues("weather.txt", text, func(key, value string) error {
		field := fields[key]
		if field == nil {
			return fmt.Errorf("unknown key %q (keys: name office area station latitude longitude)", key)
		}
		*field = value
		return nil
	})
	if err != nil {
		return c, err
	}
	if c.Office == "" || c.Area == "" || c.Station == "" {
		return c, fmt.Errorf("weather.txt needs office, area and station\n%s", weatherConfigHelp)
	}
	return c, nil
}

// parseKeyValues reads a settings file of "key = value" lines (# starts a
// comment, blank lines are skipped) and calls set for each. Errors carry the
// file name and line.
func parseKeyValues(file, text string, set func(key, value string) error) error {
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s line %d: want key = value", file, n)
		}
		if err := set(strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
			return fmt.Errorf("%s line %d: %w", file, n, err)
		}
	}
	return nil
}

func loadWeatherConfig() (weatherConfig, error) {
	data, err := os.ReadFile(weatherConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return weatherConfig{}, fmt.Errorf("no %s\n%s", weatherConfigPath(), weatherConfigHelp)
	}
	if err != nil {
		return weatherConfig{}, err
	}
	return parseWeatherConfig(string(data))
}

// dayForecast is what is read out for one day. A nil value is unknown and
// left out; nothing is guessed.
type dayForecast struct {
	Label   string // 今日 / 今夜 / 明日
	Weather string
	Max     *int
	Min     *int
	Pop     *int     // highest chance of rain over the day's remaining 6-hour blocks
	Now     *float64 // current temperature (today only, from Open-Meteo)
}

// jmaReport is the part of the forecast JSON we read: the first report (the
// short-range forecast) and its three time series.
type jmaReport struct {
	ReportDatetime string      `json:"reportDatetime"`
	TimeSeries     []jmaSeries `json:"timeSeries"`
}

type jmaSeries struct {
	TimeDefines []string  `json:"timeDefines"`
	Areas       []jmaArea `json:"areas"`
}

type jmaArea struct {
	Area struct {
		Code string `json:"code"`
	} `json:"area"`
	Weathers []string `json:"weathers"`
	Pops     []string `json:"pops"`
	Temps    []string `json:"temps"`
}

// area returns the series' values for code, or nil.
func (s jmaSeries) area(code string) *jmaArea {
	for i := range s.Areas {
		if s.Areas[i].Area.Code == code {
			return &s.Areas[i]
		}
	}
	return nil
}

// parseJMAForecast reads today and tomorrow (as of now) for c's area and
// station. now, not the report time, decides which day is today, so a
// forecast cached across midnight is still read for the right day.
//
// Temperatures come as (time, value) pairs: 00:00 is the morning low and
// 09:00 the daytime high of that date. Once the morning has passed (the 11:00
// and 17:00 reports), JMA fills the issue date's 00:00 slot with the high, so
// a low for the issue date is used only from a report issued before 09:00.
func parseJMAForecast(data []byte, c weatherConfig, now time.Time) (today, tomorrow dayForecast, err error) {
	var reports []jmaReport
	if err := json.Unmarshal(data, &reports); err != nil {
		return today, tomorrow, fmt.Errorf("forecast JSON: %w", err)
	}
	if len(reports) == 0 || len(reports[0].TimeSeries) < 3 {
		return today, tomorrow, errors.New("forecast JSON: unexpected layout")
	}
	r := reports[0]
	issued, err := time.Parse(time.RFC3339, r.ReportDatetime)
	if err != nil {
		return today, tomorrow, fmt.Errorf("forecast JSON: reportDatetime: %w", err)
	}
	weathers, pops := r.TimeSeries[0].area(c.Area), r.TimeSeries[1].area(c.Area)
	if weathers == nil || pops == nil {
		return today, tomorrow, fmt.Errorf("forecast JSON: area %s not found (check weather.txt)", c.Area)
	}
	temps := r.TimeSeries[2].area(c.Station)
	if temps == nil {
		return today, tomorrow, fmt.Errorf("forecast JSON: station %s not found (check weather.txt)", c.Station)
	}

	now = now.In(issued.Location())
	day0 := now.Format("2006-01-02")
	day1 := now.AddDate(0, 0, 1).Format("2006-01-02")
	issueDay := issued.Format("2006-01-02")
	today.Label, tomorrow.Label = "今日", "明日"
	// dayOf returns the day a time define belongs to, or nil.
	dayOf := func(at string) (*dayForecast, time.Time) {
		t, err := time.Parse(time.RFC3339, at)
		switch {
		case err != nil:
			return nil, t
		case t.Format("2006-01-02") == day0:
			return &today, t
		case t.Format("2006-01-02") == day1:
			return &tomorrow, t
		}
		return nil, t
	}

	// Weather for the rest of the day (from 17:00: tonight).
	for j, at := range r.TimeSeries[0].TimeDefines {
		if d, t := dayOf(at); d != nil && j < len(weathers.Weathers) {
			d.Weather = speakableWeather(weathers.Weathers[j])
			if d == &today && t.Hour() >= 17 {
				d.Label = "今夜"
			}
		}
	}
	// Chance of rain per 6-hour block: the highest of the blocks not over yet.
	for j, at := range r.TimeSeries[1].TimeDefines {
		d, t := dayOf(at)
		if d == nil || !t.Add(6*time.Hour).After(now) {
			continue
		}
		if v, ok := jmaNumber(pops.Pops, j); ok && (d.Pop == nil || v > *d.Pop) {
			d.Pop = &v
		}
	}
	// 00:00 low, 09:00 high.
	for j, at := range r.TimeSeries[2].TimeDefines {
		d, t := dayOf(at)
		v, ok := jmaNumber(temps.Temps, j)
		switch {
		case d == nil || !ok:
		case t.Hour() == 9:
			d.Max = &v
		case t.Hour() == 0 && (t.Format("2006-01-02") != issueDay || issued.Hour() < 9):
			d.Min = &v
		}
	}
	if today.Label == "今夜" {
		today.Max = nil // the daytime high is over
	}
	if today.Weather == "" {
		return today, tomorrow, errors.New("forecast JSON: no weather for today")
	}
	return today, tomorrow, nil
}

func jmaNumber(values []string, i int) (int, bool) {
	if i >= len(values) {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(values[i]))
	return v, err == nil
}

// speakableWeather joins JMA's space-separated words: "くもり　時々　晴れ" →
// "くもり時々晴れ".
//
// Some words are rewritten so VOICEVOX reads them right once joined: 所により
// becomes しょにより after a word, 後 is のち in forecasts (not ご), 一時雨
// becomes いちじう, 平地 ひらち. A kana word right after a kanji-kana word
// merges with it (晴れところ → はれれ), hence the leading "、".
func speakableWeather(s string) string {
	var b strings.Builder
	for w := range strings.FieldsSeq(s) { // splits on the full-width space too
		if r, ok := jmaReadings[w]; ok {
			w = r
		}
		b.WriteString(w)
	}
	out := b.String()
	for strings.Contains(out, "、、") { // 一時 + 所により
		out = strings.ReplaceAll(out, "、、", "、")
	}
	return strings.Trim(out, "、")
}

var jmaReadings = map[string]string{
	"所により": "、ところにより",
	"後":    "のち",
	"一時":   "一時、",
	"平地":   "、へいち",
}

func degrees(v int) string {
	if v < 0 {
		return fmt.Sprintf("マイナス%d度", -v)
	}
	return fmt.Sprintf("%d度", v)
}

// subject is "今日の東京" (or "今日" without a name).
func (d dayForecast) subject(name string) string {
	if name == "" {
		return d.Label
	}
	return d.Label + "の" + name
}

// temps lists the known temperatures, the current one labeled nowLabel.
func (d dayForecast) temps(nowLabel string) []string {
	var t []string
	if d.Now != nil {
		t = append(t, nowLabel+degrees(int(math.Round(*d.Now))))
	}
	if d.Max != nil {
		t = append(t, "最高"+degrees(*d.Max))
	}
	if d.Min != nil {
		t = append(t, "最低"+degrees(*d.Min))
	}
	return t
}

// sky is the weather only: "今日の東京は、くもり時々晴れ。".
func (d dayForecast) sky(name string) string {
	return d.subject(name) + "は、" + d.Weather + "。"
}

// tempText is the temperatures only: "今日の東京の気温は、いま22度、最高27度。".
func (d dayForecast) tempText(name string) string {
	t := d.temps("いま")
	if len(t) == 0 {
		return d.subject(name) + "の気温の予報は、まだ出ていない。"
	}
	return d.subject(name) + "の気温は、" + strings.Join(t, "、") + "。"
}

// rainText is the chance of rain only: "今日の東京の降水確率は30パーセント。".
func (d dayForecast) rainText(name string) string {
	if d.Pop == nil {
		return d.subject(name) + "の降水確率の予報は、まだ出ていない。"
	}
	return fmt.Sprintf("%sの降水確率は%dパーセント。", d.subject(name), *d.Pop)
}

// sentence is everything known: "今日の東京は、くもり時々晴れ。いまの気温は
// 22度、最高27度、最低18度。降水確率は30パーセント。".
func (d dayForecast) sentence(name string) string {
	s := d.sky(name)
	if t := d.temps("いまの気温は"); len(t) > 0 {
		s += strings.Join(t, "、") + "。"
	}
	if d.Pop != nil {
		s += fmt.Sprintf("降水確率は%dパーセント。", *d.Pop)
	}
	return s
}

// parseOpenMeteoCurrent reads current.temperature_2m.
func parseOpenMeteoCurrent(data []byte) (float64, error) {
	var r struct {
		Current struct {
			Temperature *float64 `json:"temperature_2m"`
		} `json:"current"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return 0, fmt.Errorf("Open-Meteo JSON: %w", err)
	}
	if r.Current.Temperature == nil {
		return 0, errors.New("Open-Meteo JSON: no current.temperature_2m")
	}
	return *r.Current.Temperature, nil
}

// weatherSource fetches over HTTP and keeps answers for a while, so asking
// twice does not fetch twice (the forecast changes three times a day).
type weatherSource struct {
	jmaURL       string // with %s for the office
	openMeteoURL string // with %s for latitude, longitude
	client       *http.Client

	mu       sync.Mutex
	forecast cached
	current  cached
}

type cached struct {
	key  string
	at   time.Time
	data []byte
}

var weather = &weatherSource{
	jmaURL:       "https://www.jma.go.jp/bosai/forecast/data/forecast/%s.json",
	openMeteoURL: "https://api.open-meteo.com/v1/forecast?latitude=%s&longitude=%s&current=temperature_2m&timezone=Asia%%2FTokyo",
	client:       &http.Client{Timeout: 5 * time.Second},
}

// How long an answer is kept: the forecast is issued three times a day.
const (
	forecastTTL = 30 * time.Minute
	currentTTL  = 10 * time.Minute
)

func (w *weatherSource) get(slot *cached, url string, ttl time.Duration) ([]byte, error) {
	w.mu.Lock()
	if slot.key == url && time.Since(slot.at) < ttl {
		data := slot.data
		w.mu.Unlock()
		return data, nil
	}
	w.mu.Unlock()

	resp, err := w.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	w.mu.Lock()
	*slot = cached{key: url, at: time.Now(), data: data}
	w.mu.Unlock()
	return data, nil
}

// days returns today's and tomorrow's forecast, today with the current
// temperature (both fetched at once). A missing current temperature is only
// left out, with a note to warn; a missing forecast is an error.
func (w *weatherSource) days(c weatherConfig, now time.Time, warn io.Writer) (today, tomorrow dayForecast, err error) {
	type reading struct {
		v   float64
		err error
	}
	var current chan reading
	if c.Latitude != "" && c.Longitude != "" {
		current = make(chan reading, 1)
		go func() {
			data, err := w.get(&w.current, fmt.Sprintf(w.openMeteoURL, c.Latitude, c.Longitude), currentTTL)
			var v float64
			if err == nil {
				v, err = parseOpenMeteoCurrent(data)
			}
			current <- reading{v, err}
		}()
	}
	data, err := w.get(&w.forecast, fmt.Sprintf(w.jmaURL, c.Office), forecastTTL)
	if err != nil {
		return today, tomorrow, fmt.Errorf("weather: %w", err)
	}
	if today, tomorrow, err = parseJMAForecast(data, c, now); err != nil {
		return today, tomorrow, fmt.Errorf("weather: %w", err)
	}
	if current != nil {
		if r := <-current; r.err != nil {
			fmt.Fprintf(warn, "warning: weather: current temperature left out: %v\n", r.err)
		} else {
			today.Now = &r.v
		}
	}
	return today, tomorrow, nil
}

// weatherFailed is said instead of a {weather...} placeholder when the
// forecast cannot be read; nothing is guessed.
const weatherFailed = "ごめんね、天気が取れなかった。"

// weatherParts are the placeholders for a day: {weather<part>} for today and
// {weather<part>_tomorrow} for tomorrow, each a sentence of its own.
var weatherParts = []struct {
	suffix string
	text   func(d dayForecast, name string) string
}{
	{"", dayForecast.sentence},      // everything
	{"_sky", dayForecast.sky},       // the weather only
	{"_temp", dayForecast.tempText}, // the temperatures only
	{"_rain", dayForecast.rainText}, // the chance of rain only
}

// expandWeather fills the weather placeholders ({weather}, {weather_sky},
// {weather_temp}, {weather_rain} and their _tomorrow forms). A placeholder
// that cannot be filled becomes weatherFailed, the rest of the text stays,
// and the error is returned for the log. Notes that do not stop it (a
// missing current temperature) go to warn.
func expandWeather(text string, now time.Time, warn io.Writer) (string, error) {
	if !strings.Contains(text, "{weather") {
		return text, nil
	}
	c, err := loadWeatherConfig()
	var today, tomorrow dayForecast
	if err == nil {
		today, tomorrow, err = weather.days(c, now, warn)
	}
	todayOK := err == nil
	tomorrowOK := todayOK && tomorrow.Weather != ""
	if todayOK && !tomorrowOK && strings.Contains(text, "_tomorrow}") {
		err = errors.New("weather: no forecast for tomorrow yet")
	}
	var pairs []string
	for _, p := range weatherParts {
		t, tm := weatherFailed, weatherFailed
		if todayOK {
			t = p.text(today, c.Name)
		}
		if tomorrowOK {
			tm = p.text(tomorrow, c.Name)
		}
		pairs = append(pairs, "{weather"+p.suffix+"}", t, "{weather"+p.suffix+"_tomorrow}", tm)
	}
	return strings.NewReplacer(pairs...).Replace(text), err
}
