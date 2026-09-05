package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

type cardAction struct{ Label, URL, Class string }
type hiddenField struct{ Name, Value string }
type postAction struct {
	Label, URL, Class string
	Fields            []hiddenField
}
type listCard struct {
	Title, Subtitle, Status, StatusClass, URL string
	Fields                                    []labelValue
	Actions                                   []cardAction
	PostActions                               []postAction
}
type listData struct {
	BaseData
	Eyebrow, Heading, Description, CreateURL, CreateLabel, EmptyMessage string
	Cards                                                               []listCard
}

type selectOption struct {
	Value, Label string
	Selected     bool
}
type formField struct {
	Name, Label, Type, Value, Placeholder, Help, Step, Min, Max string
	Required, Checked                                           bool
	Options                                                     []selectOption
}
type formSection struct {
	Title, Help string
	Fields      []formField
}
type formData struct {
	BaseData
	Eyebrow, Heading, Description, CancelURL, ActionURL, SubmitLabel, FormError string
	Sections                                                                    []formSection
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
		return "That name, inbox, browser profile, or exclusive source is already in use."
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
	return message
}
