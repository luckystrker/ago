// Command structured_output mirrors the Python "02 · Recipe builder" example:
// a culinary agent that rescales a classic carbonara from 2 servings to 6. Two
// Agent features are on display, in one story:
//
//  1. an ordinary function tool (scale_ingredient) the model calls for every
//     ingredient it needs to rescale;
//  2. a typed response schema: the final answer is coerced into a Recipe value
//     instead of free-form text.
//
// The scripted model plays the unreliable assistant on purpose: its first
// answer breaks the schema (a string where the servings integer belongs), so
// agent.ContentAs rejects it, re-asks with the validation error appended, and
// decodes the corrected second answer into the Recipe struct. The rejection,
// the re-ask and the typed result are all visible in the printed run.
//
// The "LLM" is a scripted test config, so the example runs offline without API
// keys. Run: go run ./examples/structured_output
package main

import (
	"context"
	"fmt"
	"math"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/response"
	agotest "github.com/luckystrker/ago/testing"
	"github.com/luckystrker/ago/tools"
)

// Ingredient and Recipe play the part of the Pydantic models in the original.
// The json tags fix the field names on the wire; the agschema tags carry the
// descriptions that end up in the JSON schema shown to the model.
type Ingredient struct {
	Name     string  `json:"name"`
	Quantity float64 `json:"quantity"`
	Unit     string  `json:"unit"`
}

type Recipe struct {
	Title       string       `json:"title" agschema:"description=Short human title for the recipe."`
	Servings    int          `json:"servings" agschema:"description=How many portions this recipe yields."`
	Ingredients []Ingredient `json:"ingredients"`
	Steps       []string     `json:"steps" agschema:"description=Ordered preparation steps."`
}

func main() {
	// scale_ingredient: a plain Go function wrapped as a tool. The fields of the
	// argument struct become the JSON-schema parameters the model fills in.
	scale, err := tools.NewFunctionTool("scale_ingredient",
		"Return quantity multiplied by factor, rounded to 2 decimals. Use it for every ingredient when rescaling a recipe.",
		func(args struct {
			Quantity float64 `json:"quantity" agschema:"description=Quantity from the original recipe."`
			Factor   float64 `json:"factor" agschema:"description=New servings divided by original servings."`
		}) (float64, error) {
			scaled := math.Round(args.Quantity*args.Factor*100) / 100
			fmt.Printf("  [tool] scale_ingredient(%g, %g) = %g\n", args.Quantity, args.Factor, scaled)
			return scaled, nil
		})
	if err != nil {
		fmt.Println("build scale_ingredient:", err)
		return
	}

	// Scripted model behavior:
	//   call 1 — requests all four rescalings in one parallel tool batch;
	//   call 2 — answers with a Recipe that fails validation ("six" is not an
	//            integer); this is what the ContentAs retry rejects;
	//   call 3 — the corrected Recipe, which validates and decodes.
	cfg := agotest.NewTestConfig(
		&events.ModelResponse{
			ToolCalls: events.ToolCallsEvent{Calls: []events.ToolCallEvent{
				{ID: "call-1", Name: "scale_ingredient", Arguments: `{"quantity":200,"factor":3}`},
				{ID: "call-2", Name: "scale_ingredient", Arguments: `{"quantity":2,"factor":3}`},
				{ID: "call-3", Name: "scale_ingredient", Arguments: `{"quantity":100,"factor":3}`},
				{ID: "call-4", Name: "scale_ingredient", Arguments: `{"quantity":50,"factor":3}`},
			}},
			Usage: events.Usage{PromptTokens: events.I64(180), CompletionTokens: events.I64(60), TotalTokens: events.I64(240)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: `{"title":"Carbonara for six","servings":"six","ingredients":[{"name":"spaghetti","quantity":600,"unit":"g"}],"steps":["Cook."]}`},
			Usage:   events.Usage{PromptTokens: events.I64(320), CompletionTokens: events.I64(40), TotalTokens: events.I64(360)},
		},
		&events.ModelResponse{
			Message: &events.ModelMessage{Content: `{"title":"Classic Carbonara, rescaled","servings":6,"ingredients":[` +
				`{"name":"spaghetti","quantity":600,"unit":"g"},` +
				`{"name":"eggs","quantity":6,"unit":"pieces"},` +
				`{"name":"guanciale","quantity":300,"unit":"g"},` +
				`{"name":"pecorino romano","quantity":150,"unit":"g"}],` +
				`"steps":["Boil the spaghetti in well-salted water until al dente.",` +
				`"Crisp the guanciale in a cold-started pan until the fat renders.",` +
				`"Whisk the eggs with the pecorino romano into a creamy paste.",` +
				`"Toss the drained pasta with the guanciale off the heat.",` +
				`"Fold in the egg mixture; the residual heat thickens it into a sauce."]}`},
			Usage: events.Usage{PromptTokens: events.I64(420), CompletionTokens: events.I64(80), TotalTokens: events.I64(500)},
		},
	)

	// The response schema turns the final answer into a typed Recipe: its JSON
	// schema tells the model what shape to produce, and validation holds the
	// answer to that shape before ContentAs hands it over.
	recipeSchema := response.NewSchema[Recipe]("Recipe", "A complete recipe with scaled ingredients and ordered preparation steps.")

	chef := agent.New("chef",
		"You are a culinary assistant. When asked to rescale a recipe, use the "+
			"scale_ingredient tool for every ingredient to compute the new quantity. "+
			"Return a complete Recipe object.",
		agent.WithConfig(cfg),
		agent.WithTools(scale),
		agent.WithResponseSchema(recipeSchema),
	)

	fmt.Println("== Recipe builder — scale an existing dish for 6 servings ==")
	reply, err := chef.Ask(context.Background(),
		"Start from classic carbonara for 2 servings: 200g spaghetti, 2 eggs, "+
			"100g guanciale, 50g pecorino romano. Rescale it for 6 servings and "+
			"produce the full Recipe.")
	if err != nil {
		fmt.Println("ask failed:", err)
		return
	}

	// The raw body does not validate: servings arrived as a string. Showing the
	// rejection once makes the retry below observable (Python parity: the
	// content() call raising before the retry is spent).
	if _, err := reply.Content(); err != nil {
		fmt.Println("\n[validate] first answer rejected:", err)
	}

	// ContentAs validates the answer against the response schema and decodes it
	// into Recipe. retries=1 mirrors reply.content(retries=1) in the original:
	// on a validation failure the model is re-asked once, with the error text
	// appended, and the corrected answer is decoded instead.
	recipe, err := agent.ContentAs[Recipe](context.Background(), reply, 1)
	if err != nil {
		fmt.Println("structured answer failed:", err)
		return
	}

	fmt.Printf("\n%s  (%d servings)\n", recipe.Title, recipe.Servings)
	fmt.Println()
	fmt.Println("Ingredients:")
	for _, ing := range recipe.Ingredients {
		fmt.Printf("  - %g %s %s\n", ing.Quantity, ing.Unit, ing.Name)
	}
	fmt.Println()
	fmt.Println("Steps:")
	for i, step := range recipe.Steps {
		fmt.Printf("  %d. %s\n", i+1, step)
	}

	fmt.Println("\n== transcript (tool round, rejected answer, re-ask, final) ==")
	evs, err := reply.History().GetEvents()
	if err != nil {
		fmt.Println("history failed:", err)
		return
	}
	printTranscript(evs)
}

// printTranscript renders the event kinds that make up this conversation.
func printTranscript(evs []events.Event) {
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelRequest:
			fmt.Printf("[user]      %s\n", events.RenderForPrompt(e))
		case events.ModelResponse:
			if e.Content() != "" {
				fmt.Printf("[assistant] %s\n", e.Content())
			}
		case events.ToolCallsEvent:
			for _, c := range e.Calls {
				fmt.Printf("[model]     tool call %s(%s)\n", c.Name, c.Arguments)
			}
		case events.ToolResultsEvent:
			for _, r := range e.Results {
				fmt.Printf("[tool]      %s -> %s\n", r.Name, renderParts(r.Result))
			}
		}
	}
}

func renderParts(r events.ToolResult) string {
	out := ""
	for i, p := range r.Parts {
		if i > 0 {
			out += "; "
		}
		out += events.RenderForPrompt(p)
	}
	return out
}
