// Package darkbloom fetches the Darkbloom model list (api.darkbloom.dev) and
// maps each model onto the OpenRouter model it serves, so Darkbloom-direct
// prices compete in the same rankings (see discounts.Direct). One
// authenticated GET carries prices, limits, features and live capacity.
package darkbloom

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/tomaszjudym/openrouter-discounter/internal/discounts"
	"github.com/tomaszjudym/openrouter-discounter/internal/fetch"
	"github.com/tomaszjudym/openrouter-discounter/internal/match"
)

// BaseURL is the Darkbloom OpenAI-compatible API root.
const BaseURL = "https://api.darkbloom.dev/v1"

// SourceName labels Darkbloom errors in logs and the report.
const SourceName = "Darkbloom"

// Model is one /v1/models entry. Pricing is USD per token, as OpenRouter
// lists it. SupportedFeatures is nil when the API lists none (gpt-oss-20b
// calls tools all the same, see Tools).
type Model struct {
	ID                string            `json:"id"`
	HuggingFaceID     string            `json:"hugging_face_id"`
	InputModalities   []string          `json:"input_modalities"`
	ContextLength     int               `json:"context_length"`
	MaxOutputLength   int               `json:"max_output_length"`
	Pricing           discounts.Pricing `json:"pricing"`
	SupportedFeatures []string          `json:"supported_features"`
	SamplingParams    []string          `json:"supported_sampling_parameters"`
	Metadata          struct {
		CanAccept         bool `json:"can_accept"`
		RoutableProviders int  `json:"routable_providers"`
	} `json:"metadata"`
}

// Routable reports whether any Mac can take a request for the model now.
// Darkbloom runs on shared Macs, so availability changes by the hour.
func (m Model) Routable() bool {
	return m.Metadata.CanAccept && m.Metadata.RoutableProviders > 0
}

// Tools reports whether the model calls tools: listed in its features, or,
// when it lists no features at all, as the OpenRouter model it serves does.
func (m Model) Tools(base discounts.Model) bool {
	if m.SupportedFeatures == nil {
		return base.ToolCalling()
	}
	return slices.Contains(m.SupportedFeatures, "tools")
}

type response struct {
	Data []Model `json:"data"`
}

// Fetch returns the routable models. Error text is sanitized: the key is
// removed and the message truncated.
func Fetch(ctx context.Context, g fetch.Getter, base, key string) ([]Model, error) {
	if key == "" {
		return nil, errors.New("DARKBLOOM_API_KEY not set")
	}
	hdr := http.Header{"Authorization": {"Bearer " + key}}
	body, err := g.Get(ctx, base+"/models", hdr, key)
	if err != nil {
		return nil, err
	}
	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, errors.New(fetch.Sanitize(fmt.Sprintf("unparseable response: %v", err), key))
	}
	out := make([]Model, 0, len(r.Data))
	for _, m := range r.Data {
		if m.Routable() {
			out = append(out, m)
		}
	}
	return out, nil
}

// Resolve maps each Darkbloom model onto the OpenRouter catalog and returns
// the mapped ones as direct models; unmapped holds the ids left out (no
// OpenRouter base means no benchmark score and no price to compare with).
func Resolve(models []Model, catalog []discounts.Model) (direct []discounts.DirectModel, unmapped []string) {
	byID := make(map[string]discounts.Model, len(catalog))
	for _, m := range catalog {
		byID[m.ID] = m
	}
	idx := newIndex(catalog)
	for _, m := range models {
		id, ok := idx.find(m.HuggingFaceID, m.ID)
		if !ok {
			unmapped = append(unmapped, m.ID)
			continue
		}
		direct = append(direct, discounts.DirectModel{
			ID: m.ID, BaseID: id, Pricing: m.Pricing, Tools: m.Tools(byID[id]),
			Vision:  slices.Contains(m.InputModalities, "image"),
			Context: m.ContextLength, MaxOutput: m.MaxOutputLength,
		})
	}
	return direct, unmapped
}

// index looks up OpenRouter base ids (no ":variant", no "~" alias) by their
// Hugging Face repo and by build-stripped name keys.
type index struct {
	ids  []string
	hf   map[string]string // lowercased HF repo → id
	keys map[string]string // id → space-joined keys (HF tail, id tail)
}

func newIndex(catalog []discounts.Model) index {
	x := index{hf: make(map[string]string), keys: make(map[string]string)}
	for _, m := range catalog {
		if strings.ContainsAny(m.ID, ":~") {
			continue
		}
		x.ids = append(x.ids, m.ID)
		var ks []string
		if m.HuggingFaceID != "" {
			x.hf[strings.ToLower(m.HuggingFaceID)] = m.ID
			ks = append(ks, key(m.HuggingFaceID))
		}
		ks = append(ks, key(m.ID))
		x.keys[m.ID] = strings.Join(ks, " ")
	}
	slices.Sort(x.ids)
	return x
}

// find tries, strongest evidence first across all names: the exact HF
// repo, then an equal name key, then a unique id whose key extends the
// name's key (Darkbloom drops size suffixes: "gemma-4-26b" serves
// "gemma-4-26b-a4b-it"). Names are the Darkbloom HF id and model id.
func (x index) find(names ...string) (string, bool) {
	for _, n := range names {
		if id, ok := x.hf[strings.ToLower(n)]; ok && n != "" {
			return id, true
		}
	}
	for _, prefix := range []bool{false, true} {
		for _, n := range names {
			k := key(n)
			if len(k) < minKeyLen {
				continue
			}
			if id, ok := x.unique(k, prefix); ok {
				return id, true
			}
		}
	}
	return "", false
}

// minKeyLen keeps short, generic names from matching by prefix.
const minKeyLen = 6

// unique returns the only id with a key equal to k (prefix: starting with
// k); none or several is no match.
func (x index) unique(k string, prefix bool) (string, bool) {
	found := ""
	for _, id := range x.ids {
		hit := slices.ContainsFunc(strings.Fields(x.keys[id]), func(c string) bool {
			if prefix {
				return strings.HasPrefix(c, k)
			}
			return c == k
		})
		if !hit {
			continue
		}
		if found != "" {
			return "", false
		}
		found = id
	}
	return found, found != ""
}

// buildTokens are trailing name tokens that name a build, not a model:
// quantization, packaging, speculative-decoding heads.
var buildTokens = map[string]bool{
	"gguf": true, "mlx": true, "mtp": true, "qat": true,
	"2bit": true, "3bit": true, "4bit": true, "6bit": true, "8bit": true,
	"int4": true, "int8": true, "fp4": true, "fp8": true, "nvfp4": true,
	"mxfp4": true, "mxfp8": true, "bf16": true, "fp16": true,
}

// key is a name's comparison key: the repo or id tail after "/", trailing
// build tokens dropped, then match.Normalize'd.
func key(name string) string {
	if _, tail, ok := strings.Cut(name, "/"); ok {
		name = tail
	}
	toks := strings.Split(strings.ToLower(name), "-")
	for len(toks) > 1 && buildTokens[toks[len(toks)-1]] {
		toks = toks[:len(toks)-1]
	}
	return match.Normalize(strings.Join(toks, "-"))
}
