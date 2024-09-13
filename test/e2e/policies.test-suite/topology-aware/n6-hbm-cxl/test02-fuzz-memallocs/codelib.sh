container-exit0() {
    # Terminate a container by killing the "sleep inf" child process in
    # echo CONTNAME $(sleep inf)
    local contname="$1"
    vm-command "contpid=\$(ps axf | grep -A1 'echo $contname' | grep -v grep | tail -n 1 | awk '{print \$1}'); ( set -x; kill -KILL \$contpid; )"
}

container-signal() {
    local contname="$1"
    local signal="$2"
    vm-command "pkill -$signal -f 'echo $contname'"
}
