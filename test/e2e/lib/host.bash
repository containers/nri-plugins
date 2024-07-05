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

host-wait-vm-ssh-server() {
    local _vagrantdir="$1"
    local _deadline=${deadline:-}
    local _once=1

    if [ -z "$_vagrantdir" ]; then
        echo 1>&2 "host-wait-vm-ssh-server: missing vagrant directory"
        return 1
    fi

    if [ -z "$_deadline" ]; then
        _deadline=$(( $(date +%s) + ${timeout:-30} ))
    fi

    while [ -n "$_once" ] || (( $(date +%s) < $_deadline )); do
        if [ ! -f $_vagrantdir/.ssh-config ]; then
            sleep 1
        else
            $SSH -o ConnectTimeout=1 node true
            if [ $? = 0 ]; then
                return 0
            fi
        fi
        _once=""
    done

    echo 1>&2 "host-wait-vm-ssh-server: timeout waiting for $_vagrantdir ssh server"
    return 1
}
