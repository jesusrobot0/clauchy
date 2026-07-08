package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// waybarOutput is the JSON contract Waybar expects from a custom module
// with "return-type": "json".
type waybarOutput struct {
	Text    string `json:"text"`
	Tooltip string `json:"tooltip,omitempty"`
	Class   string `json:"class,omitempty"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "waybar" {
		runWaybar()
		return
	}
	runDashboard()
}

// runWaybar prints one JSON line for the Waybar custom module.
func runWaybar() {
	out := waybarOutput{
		Text:    "󰚩",
		Tooltip: "clauchy — Claude Code monitor",
	}
	json.NewEncoder(os.Stdout).Encode(out)
}

// runDashboard renders the TUI dashboard (placeholder for now).
func runDashboard() {
	fmt.Println("clauchy dashboard — coming soon")
}
