package main

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
)

// chunk is one indexed document fragment with its embedding.
type chunk struct {
	Text      string    `json:"text"`
	Embedding []float64 `json:"embedding"`
	DocName   string    `json:"doc_name"`
}

// scoredChunk is a retrieval hit with its cosine similarity to the query.
type scoredChunk struct {
	Text    string  `json:"text"`
	Score   float64 `json:"score"`
	DocName string  `json:"doc_name"`
}

// vectorIndex is a small in-memory vector store with linear-scan top-k search.
// Sized for a homelab (hundreds to low thousands of chunks), an ANN index would
// be overkill. Persisted to a JSON file when INDEX_FILE is set.
type vectorIndex struct {
	mu     sync.Mutex
	chunks []chunk
	max    int // 0 = unlimited
}

func newVectorIndex(max int) *vectorIndex { return &vectorIndex{max: max} }

func (v *vectorIndex) Add(cs ...chunk) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.chunks = append(v.chunks, cs...)
	if v.max > 0 && len(v.chunks) > v.max {
		v.chunks = v.chunks[len(v.chunks)-v.max:]
	}
}

func (v *vectorIndex) Len() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.chunks)
}

// Search returns the top-k chunks by cosine similarity to q, best first.
func (v *vectorIndex) Search(q []float64, k int) []scoredChunk {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k <= 0 {
		return nil
	}
	scored := make([]scoredChunk, 0, len(v.chunks))
	for _, c := range v.chunks {
		scored = append(scored, scoredChunk{Text: c.Text, Score: cosine(q, c.Embedding), DocName: c.DocName})
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if k > len(scored) {
		k = len(scored)
	}
	return scored[:k]
}

func (v *vectorIndex) snapshot() []chunk {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]chunk, len(v.chunks))
	copy(out, v.chunks)
	return out
}

// Save persists the index as JSON, atomically (write temp, then rename).
func (v *vectorIndex) Save(path string) error {
	data, err := json.Marshal(v.snapshot())
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads a persisted index, replacing the in-memory chunks.
func (v *vectorIndex) Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cs []chunk
	if err := json.Unmarshal(data, &cs); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.chunks = cs
	return nil
}

// docIndex is the process-wide retrieval index, replaced by main() if an
// INDEX_FILE is configured.
var docIndex = newVectorIndex(0)
