package controlplane

import (
	"context"
	"strings"
)

var webImageModels = []string{"gpt-image-2", "gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"}

func (s *Server) modelCatalog(ctx context.Context) (Map, error) {
	raw, e := s.DB.Document(ctx, "system_settings")
	if e != nil {
		return nil, e
	}
	catalog, generation := object(raw["model_catalog"]), object(raw["image_generation"])
	chat := unique(stringsOf(catalog["chat_models"]))
	if len(chat) == 0 {
		for _, k := range []string{"base_chat_models", "specialized_chat_models", "image_capable_chat_models"} {
			chat = unique(append(chat, stringsOf(catalog[k])...))
		}
	}
	chatSource := "config"
	if len(chat) == 0 {
		chatSource = "fallback"
		chat = []string{"gpt-5.6", "auto"}
	}
	images := []string{}
	for _, v := range []any{generation["model_options"], catalog["image_api_models"], generation["supported_models"]} {
		if xs := unique(stringsOf(v)); len(xs) > 0 {
			images = xs
			break
		}
	}
	imageSource := "config"
	if len(images) == 0 {
		items, e := s.Accounts.List(ctx)
		if e != nil {
			return nil, e
		}
		available := false
		types := map[string]bool{}
		for _, m := range items {
			if imageAvailable(m, s.now()) {
				available = true
				if source(m["source_type"]) == "codex" {
					types[text(plan(m["type"]))] = true
				}
			}
		}
		images = append(images, webImageModels...)
		imageSource = "fallback"
		if available {
			imageSource = "accounts"
			codex := "codex-gpt-image-2"
			if types["Plus"] || types["Team"] || types["Pro"] {
				images = append(images, codex)
			}
			for _, p := range []string{"Plus", "Team", "Pro"} {
				if types[p] {
					images = append(images, strings.ToLower(p)+"-"+codex)
				}
			}
		}
	}
	chatDefault, imageDefault := chat[0], images[0]
	for _, name := range chat {
		if name == "auto" {
			chatDefault = name
		}
	}
	high := []string{}
	for _, name := range images {
		if name == "gpt-image-2" {
			imageDefault = name
		}
		if name == "codex-gpt-image-2" || strings.HasSuffix(name, "-codex-gpt-image-2") {
			high = append(high, name)
		}
	}
	values, e := s.Settings.Values(ctx)
	if e != nil {
		return nil, e
	}
	payload := Map{"chat_models": chat, "image_models": images, "defaults": Map{"chat_model": chatDefault, "image_model": imageDefault}, "capabilities": Map{"image_upscale": values["image_upscale_enabled"], "high_resolution_image_models": high}, "source": Map{"chat": chatSource, "image": imageSource}}
	out := copyMap(payload)
	out["object"], out["schema_version"], out["generated_at"], out["revision"], out["all_models"], out["openai_models_endpoint"] = "model_catalog", 1, generated(s.now()), digest(canonical(payload))[:16], unique(append(append([]string{}, chat...), images...)), "/v1/models"
	return out, nil
}
