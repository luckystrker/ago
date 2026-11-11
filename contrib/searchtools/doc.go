// Package searchtools ports the AG2 `extensions/tools/search` provider
// toolkits (Exa, Firecrawl, Glasser, Keenable, Serply, TinyFish, Xquik) as Go
// `tools.Tool` / `tools.Toolkit` implementations.
//
// Every provider is a plain REST API called over net/http: no SDKs, no external
// dependencies, and no network access in tests (each client endpoint is
// overridable, so tests point it at an httptest server). A toolkit registers its
// whole tool set on an agent; the factories (Search, Fetch, ...) return a single
// member for subset registration.
//
// Requests are composed exactly like the Python extension: the same paths,
// query parameters, JSON keys, auth headers and response shaping. Deliberate
// differences carry an AGO-DEVIATION comment.
package searchtools
