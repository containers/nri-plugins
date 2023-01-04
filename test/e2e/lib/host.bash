source "$(dirname "${BASH_SOURCE[0]}")/command.bash"

HOST_PROMPT=${HOST_PROMPT-"\e[38;5;11mhost>\e[0m "}
HOST_LIB_DIR="$(dirname "${BASH_SOURCE[0]}")"
HOST_PROJECT_DIR="$(dirname "$(dirname "$(realpath "$HOST_LIB_DIR")")")"

host-command() {
    command-start "host" "$HOST_PROMPT" "$1"
    bash -c "$COMMAND" 2>&1 | command-handle-output
    command-end ${PIPESTATUS[0]}
    return $COMMAND_STATUS
}
