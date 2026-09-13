package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

type cardAction struct{ Label, URL, Class string }
type hiddenField struct{ Name, Value string }
type postAction struct {
	Label, URL, Class       string
	Fields                  []hiddenField
	SelectName, SelectLabel string
	SelectOptions           []selectOption
}
type listCard struct {
	Title, Subtitle, Status, StatusClass, URL string
	Description                               string
	Fields                                    []labelValue
	Actions                                   []cardAction
	PostActions                               []postAction
	Default                                   bool
}
type listData struct {
	BaseData
	Eyebrow, Heading, Description, CreateURL, CreateLabel, EmptyMessage string
	Notice                                                              string
	Cards                                                               []listCard
}

type selectOption struct {
	Value, Label string
	Selected     bool
	LakeDefaults string
}
type formField struct {
	Name, Label, Type, Value, Placeholder, Help, Step, Min, Max string
	Required, Checked                                           bool
	Wide                                                        bool
	Options                                                     []selectOption
}
type formSection struct {
	Title, Help, Class, Provider string
	HelpURL, HelpLabel           string
	Fields                       []formField
	Advanced                     bool
}
type formData struct {
	BaseData
	HiddenFields                                                                []hiddenField
	LakeSettingsURL                                                             string
	AdvancedHelp                                                                string
	SubmitHelp                                                                  string
	SubmitDisabled                                                              bool
	Eyebrow, Heading, Description, CancelURL, ActionURL, SubmitLabel, FormError string
	Sections                                                                    []formSection
	LakeSelection                                                               bool
	SourceSelection                                                             bool
}

func checked(r *http.Request, name string) bool {
	return r.Form.Get(name) == "1" || r.Form.Get(name) == "on" || r.Form.Get(name) == "true"
}
func parseInt64(value string) int64 {
	result, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return result
}
func formStatus(err string) int {
	if err != "" {
		return http.StatusUnprocessableEntity
	}
	return http.StatusOK
}
func safeFormError(err error) string {
	if errors.Is(err, store.ErrResourceLimit) {
		return "This account has reached the limit for this resource."
	}
	if errors.Is(err, store.ErrConflict) {
		return "That name or inbox is already in use, or an active job is using these settings."
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 300 {
		return "The submitted values were not accepted."
	}
	for _, marker := range []string{"password", "token", "secret", "cipher", "decrypt", "encrypt"} {
		if strings.Contains(strings.ToLower(message), marker) {
			return "The submitted provider or credential values were not accepted."
		}
	}
	return strings.NewReplacer(
		"poll timing must fit the worker bounds", "Use an availability window of 1–900 seconds and retry delays of 0.05–60 seconds. The minimum retry delay cannot exceed the maximum.",
		"auth deadline must fall within the preparation window", "The sign-in deadline must be within the preparation window.",
		"profile is required", "Choose a booking sign-in.",
		"vehicle is required", "Enter a vehicle keyword.",
	).Replace(message)
}
