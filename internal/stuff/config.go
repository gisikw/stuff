package stuff

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const readerConfigID = "config:reader"

func readerConfigEnvelope(doc Document) Document {
	out := Document{"home_item_id": nil, "revision": nil, "updated_at": nil}
	if doc == nil {
		return out
	}
	if value, ok := doc["home_item_id"].(string); ok && value != "" {
		out["home_item_id"] = value
	}
	if value, ok := doc["_rev"].(string); ok && value != "" {
		out["revision"] = value
	} else if value, ok := doc["revision"].(string); ok && value != "" {
		out["revision"] = value
	}
	if value, ok := doc["updated_at"].(string); ok && value != "" {
		out["updated_at"] = value
	}
	return out
}

func isStoreNotFound(err error) bool {
	var storeErr *StoreError
	return errors.As(err, &storeErr) && storeErr.Status == http.StatusNotFound
}

func (s *Server) getReaderConfig(w http.ResponseWriter, r *http.Request) error {
	doc, err := s.store.Get(r.Context(), readerConfigID)
	if err != nil {
		if isStoreNotFound(err) {
			writeJSON(w, http.StatusOK, readerConfigEnvelope(nil))
			return nil
		}
		return err
	}
	if doc["stuff_kind"] != "reader_config" {
		return fmt.Errorf("reserved ReaderConfig record has unexpected kind")
	}
	writeJSON(w, http.StatusOK, readerConfigEnvelope(doc))
	return nil
}

func (s *Server) requireHomeItem(ctx context.Context, id string) error {
	doc, err := s.store.Get(ctx, id)
	if err != nil {
		if isStoreNotFound(err) {
			return bad("home_item_id", fmt.Sprintf("Item %q does not exist", id), "an Item ID from `stuff add` or `stuff find`")
		}
		return err
	}
	if doc["stuff_kind"] != "item" {
		return bad("home_item_id", "home_item_id must reference an Item", "an Item ID from `stuff add` or `stuff find`")
	}
	return nil
}

func (s *Server) updateReaderConfig(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		HomeItemID optionalJSON `json:"home_item_id"`
		Revision   string       `json:"revision"`
	}
	if err := decodeJSON(r, MaxJSONBytes, &in); err != nil {
		return err
	}
	if !in.HomeItemID.Present {
		return bad("$", "update changes nothing", "provide home_item_id as an Item ID or null")
	}

	var homeID string
	clearHome := in.HomeItemID.Value == nil
	if !clearHome {
		value, ok := in.HomeItemID.Value.(string)
		if !ok {
			return bad("home_item_id", "home_item_id must be a string or null", "an existing Item ID or null")
		}
		homeID = strings.TrimSpace(value)
		if homeID == "" {
			return bad("home_item_id", "home_item_id cannot be empty", "an existing Item ID or null")
		}
		if err := s.requireHomeItem(r.Context(), homeID); err != nil {
			return err
		}
	}

	doc, err := s.store.Get(r.Context(), readerConfigID)
	if err != nil && !isStoreNotFound(err) {
		return err
	}
	if isStoreNotFound(err) {
		if clearHome {
			writeJSON(w, http.StatusOK, readerConfigEnvelope(nil))
			return nil
		}
		if in.Revision != "" {
			return bad("revision", "ReaderConfig does not exist yet", "omit revision when setting the first homepage")
		}
		now := s.now().UTC().Format(time.RFC3339Nano)
		doc = Document{"stuff_kind": "reader_config", "home_item_id": homeID, "updated_at": now}
		revision, createErr := s.store.Create(r.Context(), readerConfigID, doc)
		if createErr != nil {
			return createErr
		}
		doc["_rev"] = revision
		writeJSON(w, http.StatusOK, readerConfigEnvelope(doc))
		return nil
	}
	if doc["stuff_kind"] != "reader_config" {
		return fmt.Errorf("reserved ReaderConfig record has unexpected kind")
	}
	if clearHome {
		if _, present := doc["home_item_id"]; !present {
			writeJSON(w, http.StatusOK, readerConfigEnvelope(doc))
			return nil
		}
		delete(doc, "home_item_id")
	} else {
		doc["home_item_id"] = homeID
	}
	revision, _ := doc["_rev"].(string)
	if in.Revision != "" {
		revision = in.Revision
	}
	doc["updated_at"] = s.now().UTC().Format(time.RFC3339Nano)
	newRevision, err := s.store.Put(r.Context(), readerConfigID, revision, doc)
	if err != nil {
		return err
	}
	doc["_rev"] = newRevision
	writeJSON(w, http.StatusOK, readerConfigEnvelope(doc))
	return nil
}

func (s *Server) serveReadHome(w http.ResponseWriter, r *http.Request) {
	config, err := s.store.Get(r.Context(), readerConfigID)
	if err != nil {
		if !isStoreNotFound(err) {
			s.log.Error("reading ReaderConfig for homepage", "error", err)
		}
		http.Redirect(w, r, "/read", http.StatusSeeOther)
		return
	}
	if config["stuff_kind"] != "reader_config" {
		s.log.Error("ReaderConfig has unexpected kind")
		http.Redirect(w, r, "/read", http.StatusSeeOther)
		return
	}
	homeID, _ := config["home_item_id"].(string)
	if homeID == "" {
		http.Redirect(w, r, "/read", http.StatusSeeOther)
		return
	}
	item, err := s.store.Get(r.Context(), homeID)
	if err != nil || item["stuff_kind"] != "item" {
		if err != nil && !isStoreNotFound(err) {
			s.log.Error("reading configured homepage Item", "item", homeID, "error", err)
		}
		http.Redirect(w, r, "/read", http.StatusSeeOther)
		return
	}
	s.serveReadItem(w, r, homeID)
}
