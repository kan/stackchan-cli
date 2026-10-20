package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var jst = time.FixedZone("JST", 9*60*60)

// tokyo is the area of testdata/jma-forecast-130000-1100.json, the real
// forecast for Tokyo issued 2026-10-20 11:00.
var tokyo = weatherConfig{Name: "東京", Office: "130000", Area: "130010", Station: "44132"}

func readFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "jma-forecast-130000-1100.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func intp(v int) *int { return &v }

func checkDay(t *testing.T, name string, got, want dayForecast) {
	t.Helper()
	show := func(p *int) any {
		if p == nil {
			return nil
		}
		return *p
	}
	if got.Label != want.Label || got.Weather != want.Weather ||
		show(got.Max) != show(want.Max) || show(got.Min) != show(want.Min) || show(got.Pop) != show(want.Pop) {
		t.Errorf("%s = {%s %q max %v min %v pop %v}, want {%s %q max %v min %v pop %v}", name,
			got.Label, got.Weather, show(got.Max), show(got.Min), show(got.Pop),
			want.Label, want.Weather, show(want.Max), show(want.Min), show(want.Pop))
	}
}

func TestParseJMAForecast1100(t *testing.T) {
	today, tomorrow, err := parseJMAForecast(readFixture(t), tokyo, time.Date(2026, 10, 20, 12, 0, 0, 0, jst))
	if err != nil {
		t.Fatal(err)
	}
	// The 11:00 report fills today's 00:00 slot with the high (27): no low.
	checkDay(t, "today", today, dayForecast{Label: "今日", Weather: "くもり、ところにより雨", Max: intp(27), Pop: intp(30)})
	checkDay(t, "tomorrow", tomorrow, dayForecast{Label: "明日", Weather: "くもり時々晴れ、ところにより夜遅く雨", Max: intp(29), Min: intp(20), Pop: intp(20)})
}

// Words VOICEVOX misreads once joined are rewritten (see speakableWeather).
func TestSpeakableWeather(t *testing.T) {
	tests := map[string]string{
		"くもり　時々　晴れ":               "くもり時々晴れ",
		"くもり　所により　昼過ぎ　から　雨":       "くもり、ところにより昼過ぎから雨",
		"所により　雨":                  "ところにより雨",
		"晴れ　後　くもり":                "晴れのちくもり",
		"くもり　一時　雨":                "くもり一時、雨",
		"くもり　平地　では　雪":             "くもり、へいちでは雪",
		"くもり　一時　所により　雨":           "くもり一時、ところにより雨", // no "、、"
		"くもり　所により　夕方　まで　雨　で　雷を伴う": "くもり、ところにより夕方まで雨で雷を伴う",
	}
	for in, want := range tests {
		if got := speakableWeather(in); got != want {
			t.Errorf("speakableWeather(%q) = %q, want %q", in, got, want)
		}
	}
}

// A forecast cached across midnight is read for the new day.
func TestParseJMAForecastAfterMidnight(t *testing.T) {
	today, tomorrow, err := parseJMAForecast(readFixture(t), tokyo, time.Date(2026, 10, 21, 0, 30, 0, 0, jst))
	if err != nil {
		t.Fatal(err)
	}
	checkDay(t, "today", today, dayForecast{Label: "今日", Weather: "くもり時々晴れ、ところにより夜遅く雨", Max: intp(29), Min: intp(20), Pop: intp(20)})
	checkDay(t, "tomorrow", tomorrow, dayForecast{Label: "明日", Weather: "くもり時々晴れ"})
}

// jmaJSON builds a report with only the fields parseJMAForecast reads.
func jmaJSON(t *testing.T, issued string, weathers, pops, temps map[string]string) []byte {
	t.Helper()
	series := func(m map[string]string, field, code string) map[string]any {
		var times, values []string
		for k, v := range m {
			times = append(times, k)
			values = append(values, v)
		}
		return map[string]any{"timeDefines": times, "areas": []any{map[string]any{"area": map[string]string{"code": code}, field: values}}}
	}
	data, err := json.Marshal([]any{map[string]any{
		"reportDatetime": issued,
		"timeSeries": []any{
			series(weathers, "weathers", "130010"),
			series(pops, "pops", "130010"),
			series(temps, "temps", "44132"),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseJMAForecast0500KeepsTodaysLow(t *testing.T) {
	data := jmaJSON(t, "2026-10-20T05:00:00+09:00",
		map[string]string{"2026-10-20T05:00:00+09:00": "晴れ", "2026-10-21T00:00:00+09:00": "雨"},
		map[string]string{"2026-10-20T06:00:00+09:00": "0", "2026-10-20T12:00:00+09:00": "10"},
		map[string]string{"2026-10-20T00:00:00+09:00": "14", "2026-10-20T09:00:00+09:00": "24"})
	today, _, err := parseJMAForecast(data, tokyo, time.Date(2026, 10, 20, 6, 0, 0, 0, jst))
	if err != nil {
		t.Fatal(err)
	}
	checkDay(t, "today", today, dayForecast{Label: "今日", Weather: "晴れ", Max: intp(24), Min: intp(14), Pop: intp(10)})
}

// From 17:00 today is "tonight": the daytime high is over and not read.
func TestParseJMAForecast1700IsTonight(t *testing.T) {
	data := jmaJSON(t, "2026-10-20T17:00:00+09:00",
		map[string]string{"2026-10-20T17:00:00+09:00": "くもり　夜　雨"},
		map[string]string{"2026-10-20T18:00:00+09:00": "60"},
		map[string]string{"2026-10-20T09:00:00+09:00": "27", "2026-10-21T00:00:00+09:00": "15", "2026-10-21T09:00:00+09:00": "21"})
	today, tomorrow, err := parseJMAForecast(data, tokyo, time.Date(2026, 10, 20, 19, 0, 0, 0, jst))
	if err != nil {
		t.Fatal(err)
	}
	checkDay(t, "today", today, dayForecast{Label: "今夜", Weather: "くもり夜雨", Pop: intp(60)})
	checkDay(t, "tomorrow", tomorrow, dayForecast{Label: "明日", Max: intp(21), Min: intp(15)})
}

// A 6-hour block that is over does not count toward the chance of rain.
func TestParseJMAForecastSkipsPastRainBlocks(t *testing.T) {
	// Today's blocks: 12-18 30%, 18-24 20%.
	today, _, err := parseJMAForecast(readFixture(t), tokyo, time.Date(2026, 10, 20, 18, 30, 0, 0, jst))
	if err != nil {
		t.Fatal(err)
	}
	if today.Pop == nil || *today.Pop != 20 {
		t.Errorf("pop at 18:30 = %v, want 20 (the 12-18 block is over)", today.Pop)
	}
}

func TestParseJMAForecastErrors(t *testing.T) {
	if _, _, err := parseJMAForecast([]byte("<html>"), tokyo, time.Now()); err == nil {
		t.Error("non-JSON: want an error")
	}
	other := tokyo
	other.Area = "999999"
	if _, _, err := parseJMAForecast(readFixture(t), other, time.Date(2026, 10, 20, 12, 0, 0, 0, jst)); err == nil || !strings.Contains(err.Error(), "999999") {
		t.Errorf("unknown area: err = %v, want one naming it", err)
	}
	// A week later the report has nothing for today.
	if _, _, err := parseJMAForecast(readFixture(t), tokyo, time.Date(2026, 10, 27, 12, 0, 0, 0, jst)); err == nil {
		t.Error("stale forecast: want an error")
	}
}

func TestDayForecastTexts(t *testing.T) {
	now := 16.6
	d := dayForecast{Label: "今日", Weather: "晴れ", Max: intp(24), Min: intp(-2), Pop: intp(0), Now: &now}
	empty := dayForecast{Label: "明日", Weather: "雨"}
	tests := []struct{ name, got, want string }{
		{"sentence", d.sentence("東京"), "今日の東京は、晴れ。いまの気温は17度、最高24度、最低マイナス2度。降水確率は0パーセント。"},
		{"sky", d.sky("東京"), "今日の東京は、晴れ。"},
		{"temp", d.tempText("東京"), "今日の東京の気温は、いま17度、最高24度、最低マイナス2度。"},
		{"rain", d.rainText("東京"), "今日の東京の降水確率は0パーセント。"},
		{"sentence, nothing else known", empty.sentence(""), "明日は、雨。"},
		{"temp, not out yet", empty.tempText("東京"), "明日の東京の気温の予報は、まだ出ていない。"},
		{"rain, not out yet", empty.rainText("東京"), "明日の東京の降水確率の予報は、まだ出ていない。"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

func TestParseWeatherConfig(t *testing.T) {
	c, err := parseWeatherConfig("# comment\nname = 東京\noffice = 130000  # 東京都\narea=130010\nstation = 44132\nlatitude = 35.69\nlongitude = 139.69\n")
	if err != nil {
		t.Fatal(err)
	}
	if c != (weatherConfig{"東京", "130000", "130010", "44132", "35.69", "139.69"}) {
		t.Errorf("config = %+v", c)
	}
	if _, err := parseWeatherConfig("office = 130000\n"); err == nil {
		t.Error("missing area and station: want an error")
	}
	if _, err := parseWeatherConfig("city = 東京\n"); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("unknown key: err = %v", err)
	}
}

// fakeWeather points the weather source at a local server for the test.
func fakeWeather(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	saved := weather
	weather = &weatherSource{
		jmaURL:       srv.URL + "/forecast/%s.json",
		openMeteoURL: srv.URL + "/meteo?lat=%s&lon=%s",
		client:       srv.Client(),
	}
	t.Cleanup(func() { weather = saved })
}

func writeWeatherConfig(t *testing.T) {
	t.Helper()
	fakeHome(t)
	if err := os.MkdirAll(stackchanDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	conf := "name = 東京\noffice = 130000\narea = 130010\nstation = 44132\nlatitude = 35.69\nlongitude = 139.69\n"
	if err := os.WriteFile(weatherConfigPath(), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExpandWeather(t *testing.T) {
	writeWeatherConfig(t)
	fixture := readFixture(t)
	forecastHits := 0
	fakeWeather(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/forecast/130000.json":
			forecastHits++
			_, _ = w.Write(fixture)
		case "/meteo":
			_, _ = w.Write([]byte(`{"current":{"temperature_2m":22.4}}`))
		default:
			http.NotFound(w, r)
		}
	})
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, jst)
	got, err := expandWeather("{weather}", now, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := "今日の東京は、くもり、ところにより雨。いまの気温は22度、最高27度。降水確率は30パーセント。"; got != want {
		t.Errorf("{weather} = %q, want %q", got, want)
	}
	for placeholder, want := range map[string]string{
		"{weather_tomorrow}":      "明日の東京は、くもり時々晴れ、ところにより夜遅く雨。最高29度、最低20度。降水確率は20パーセント。",
		"{weather_sky}":           "今日の東京は、くもり、ところにより雨。",
		"{weather_temp}":          "今日の東京の気温は、いま22度、最高27度。",
		"{weather_rain}":          "今日の東京の降水確率は30パーセント。",
		"{weather_sky_tomorrow}":  "明日の東京は、くもり時々晴れ、ところにより夜遅く雨。",
		"{weather_temp_tomorrow}": "明日の東京の気温は、最高29度、最低20度。",
		"{weather_rain_tomorrow}": "明日の東京の降水確率は20パーセント。",
	} {
		got, err := expandWeather(placeholder, now, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %q, want %q", placeholder, got, want)
		}
	}
	if forecastHits != 1 {
		t.Errorf("forecast fetched %d times, want 1 (cached)", forecastHits)
	}
	if got, err := expandWeather("{date}です", now, io.Discard); err != nil || got != "{date}です" {
		t.Errorf("text without weather: %q, %v; want it unchanged", got, err)
	}
}

// Before tomorrow's forecast is out, only the tomorrow placeholders fail.
func TestExpandWeatherWithoutTomorrow(t *testing.T) {
	writeWeatherConfig(t)
	data := jmaJSON(t, "2026-10-20T05:00:00+09:00",
		map[string]string{"2026-10-20T05:00:00+09:00": "晴れ"},
		map[string]string{"2026-10-20T06:00:00+09:00": "0"},
		map[string]string{"2026-10-20T09:00:00+09:00": "24"})
	fakeWeather(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/forecast/130000.json" {
			_, _ = w.Write(data)
			return
		}
		_, _ = w.Write([]byte(`{"current":{"temperature_2m":15}}`))
	})
	got, err := expandWeather("{weather_sky}{weather_tomorrow}", time.Date(2026, 10, 20, 6, 0, 0, 0, jst), io.Discard)
	if want := "今日の東京は、晴れ。" + weatherFailed; err == nil || got != want {
		t.Errorf("got %q, %v; want %q and an error", got, err, want)
	}
}

func TestExpandWeatherWithoutCurrentTemperature(t *testing.T) {
	writeWeatherConfig(t)
	fixture := readFixture(t)
	fakeWeather(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/forecast/130000.json" {
			_, _ = w.Write(fixture)
			return
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	var warn strings.Builder
	got, err := expandWeather("{weather}", time.Date(2026, 10, 20, 12, 0, 0, 0, jst), &warn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "current temperature left out") {
		t.Errorf("warning = %q, want a note on the missing current temperature", warn.String())
	}
	if want := "今日の東京は、くもり、ところにより雨。最高27度。降水確率は30パーセント。"; got != want {
		t.Errorf("{weather} = %q, want %q (current temperature left out)", got, want)
	}
}

func TestExpandWeatherFailure(t *testing.T) {
	writeWeatherConfig(t)
	fakeWeather(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	// Only the placeholder is replaced; the rest of the text is still said.
	got, err := expandWeather("おはよう。{weather}", time.Date(2026, 10, 20, 12, 0, 0, 0, jst), io.Discard)
	if want := "おはよう。" + weatherFailed; err == nil || got != want {
		t.Errorf("got %q, %v; want %q and an error", got, err, want)
	}

	fakeHome(t) // no weather.txt
	if got, err := expandWeather("{weather}", time.Now(), io.Discard); err == nil || got != weatherFailed {
		t.Errorf("no config: got %q, %v; want %q and an error", got, err, weatherFailed)
	}
}
