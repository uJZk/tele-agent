package dispatch

import (
	"fmt"
	"path"
	"strings"
)

// NameEditor is the shim Claude starts as its editor: tele sets VISUAL to
// it. Claude passes it one file, a path in the remote view; session main
// opens a copy of that file in the user's own editor on this machine
// (docs/claude-code.md "外部编辑器与 IDE 探测").
const NameEditor = "tele-editor"

// ideScript is the script Claude runs with /bin/sh -c on Linux to find the
// IDEs running on this machine, for its IDE integration, which works with
// local IDEs only. It runs locally, like the clipboard scripts.
const ideScript = `ps aux | grep -E "code|cursor|windsurf|devin-desktop|idea|pycharm|webstorm|phpstorm|rubymine|clion|goland|rider|datagrip|dataspell|aqua|gateway|fleet|android-studio" | grep -v grep`

// editorAction returns the Action for the editor shim with arguments args:
// exactly the file, as Claude passes it (docs/claude-code.md "外部编辑器与
// IDE 探测").
func editorAction(args []string) (Action, error) {
	if len(args) != 1 || !path.IsAbs(args[0]) || strings.ContainsFunc(args[0], isControl) {
		return Action{}, fmt.Errorf("%w: %s %q: only one absolute path", ErrRejected, NameEditor, args)
	}
	return Action{Local: true, Edit: path.Clean(args[0])}, nil
}
