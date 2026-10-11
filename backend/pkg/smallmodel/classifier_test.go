package smallmodel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifier_Choose_ReturnsTopLabelAndScore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/classify", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"label":"go programming","score":0.91,"ranking":[]}`))
	}))
	defer srv.Close()

	label, score, err := NewClassifier(srv.URL).Choose(t.Context(), "compile a go binary", []string{"go programming", "python programming"}, "")
	require.NoError(t, err)
	assert.Equal(t, "go programming", label)
	assert.InDelta(t, 0.91, score, 0.0001)
}

func TestClassifier_Choose_ErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, _, err := NewClassifier(srv.URL).Choose(t.Context(), "x", []string{"a", "b"}, "")
	assert.Error(t, err)
}

func TestClassifier_Choose_ErrorsOnEmptyLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"label":"","score":0}`))
	}))
	defer srv.Close()

	_, _, err := NewClassifier(srv.URL).Choose(t.Context(), "x", []string{"a"}, "")
	assert.Error(t, err)
}

func TestClassifier_Choose_ErrorsWhenUnreachable(t *testing.T) {
	_, _, err := NewClassifier("http://127.0.0.1:0").Choose(t.Context(), "x", []string{"a"}, "")
	assert.Error(t, err)
}
