##########################################
#
# distro-agnostic interface
#
# To add a new distro implement distro-specific versions of these
# functions. You can omit implementing those which already resolve
# to an existing function which works for the new distro.
#
# To add a new API function, add an new briding resolution entry below.
#

distro-install-pkg()             { distro-resolve "$@"; }
distro-set-kernel-commandline()  { distro-resolve "$@"; }
distro-kernel-install-builddep() { distro-resolve "$@"; }
distro-kernel-fetch-sources()    { distro-resolve "$@"; }

# distro-specific function resolution
distro-resolve() {
    local apifn="${FUNCNAME[1]}" fn prefn postfn
    # shellcheck disable=SC2086
    {
        fn="$(distro-resolve-fn $apifn)"
        prefn="$(distro-resolve-fn $apifn-pre)"
        postfn="$(distro-resolve-fn $apifn-post)"
        command-debug-log "$VM_DISTRO/${FUNCNAME[1]}: pre: ${prefn:--}, fn: ${fn:--}, post: ${postfn:--}"
    }
    [ -n "$prefn" ] && { $prefn "$@" || return $?; }
    $fn "$@" || return $?
    [ -n "$postfn" ] && { $postfn "$@" || return $?; }
    return 0
}

distro-resolve-fn() {
    # We try resolving distro-agnostic implementations by looping through
    # a list of candidate function names in decreasing order of precedence
    # and returning the first one found. The candidate list has version-
    # exact and unversioned distro-specific functions and a set fallbacks
    # based on known distro, derivative, and package type relations.
    #
    # For normal functions the last fallback is 'distro-unresolved' which
    # prints and returns an error. For pre- and post-functions there is no
    # similar setup. IOW, unresolved normal distro functions fail while
    # unresolved pre- and post-functions get ignored (in distro-resolve).
    local apifn="$1" candidates fn
    case $apifn in
    distro-*) apifn="${apifn#distro-}" ;;
    *) error "internal error: can't resolve non-API function $apifn" ;;
    esac
    candidates=""
    case $distro in
    *ubuntu*) candidates="$candidates ubuntu-$apifn debian-$apifn" ;;
    *fedora*) candidates="$candidates fedora-$apifn rpm-$apifn" ;;
    esac
    case $apifn in
    *-pre | *-post) ;;
    *) candidates="$candidates default-$apifn distro-unresolved" ;;
    esac
    for fn in $candidates; do
        if [ "$(type -t -- "$fn")" = "function" ]; then
            echo "$fn"
            return 0
        fi
    done
}

# distro-unresolved terminates failed API function resolution with an error.
distro-unresolved() {
    local apifn="${FUNCNAME[2]}"
    command-error "internal error: can't resolve \"$apifn\" for \"$VM_DISTRO\""
    return 1
}

##########################################
# distro: ubuntu / debian

debian-install-pkg() {
    # dpkg configure may ask "The default action is to keep your
    # current version", for instance when a test has added
    # /etc/containerd/config.toml and then apt-get installs
    # containerd. 'yes ""' will continue with the default answer (N:
    # keep existing) in this case. Without 'yes' installation fails.

    # Add apt-get option "--reinstall" if any environment variable
    # reinstall_<pkg>=1
    local pkg
    local opts=""
    for pkg in "$@"; do
        if [ "$(eval echo \$reinstall_$pkg)" == "1" ]; then
            opts="$opts --reinstall"
            break
        fi
    done
    vm-command "yes \"\" | DEBIAN_FRONTEND=noninteractive apt-get install $opts -y --allow-downgrades $*" ||
        command-error "failed to install $*"
}

debian-kernel-install-builddep() {
    distro-install-pkg git-core build-essential bc kmod cpio flex libncurses5-dev libelf-dev libssl-dev dwarves bison
}

ubuntu-kernel-fetch-sources() {
    vm-command "git clone --depth=1 git://git.launchpad.net/~ubuntu-kernel/ubuntu/+source/linux/+git/unstable linux"
    echo "Kernel ready for patching and configuring."
    echo "build:   cd linux && make bindeb-pkg"
    echo "install: dpkg -i linux-*.deb"
}

ubuntu-set-kernel-cmdline() {
    local e2e_defaults="$*"
    vm-command "echo 'GRUB_CMDLINE_LINUX_DEFAULT=\"\${GRUB_CMDLINE_LINUX_DEFAULT} ${e2e_defaults}\"' > /etc/default/grub.d/60-e2e-defaults.cfg" || {
        command-error "writing new command line parameters failed"
    }
    vm-command "update-grub" || {
        command-error "updating grub failed"
    }
}

##########################################
# distro: fedora

fedora-install-pkg() {
    local pkg
    local do_reinstall=0
    for pkg in "$@"; do
        if [ "$(eval echo \$reinstall_$pkg)" == "1" ]; then
            do_reinstall=1
            break
        fi
    done
    vm-command "dnf install -y $*" ||
        command-error "failed to install $*"
    # When requesting reinstallation, detect which packages were
    # already installed and reinstall those.
    # (Unlike apt and zypper, dnf offers no option for reinstalling
    # existing and installing new packages on the same run.)
    if [ "$do_reinstall" == "1" ]; then
        local reinstall_pkgs
        reinstall_pkgs=$(awk -F '[ -]' -v ORS=" " '/Package .* already installed/{print $2}' <<<"$COMMAND_OUTPUT")
        if [ -n "$reinstall_pkgs" ]; then
            vm-command "dnf reinstall -y $reinstall_pkgs"
        fi
    fi
}

fedora-kernel-install-builddep() {
    fedora-install-pkg fedpkg fedora-packager rpmdevtools ncurses-devel pesign grubby git-core
    # If a vanilla kernel is going to be built, there is no kernel.spec
    # for running "dnf builddep kernel.spec". Install minimal dependencies.
    fedora-install-pkg make gcc flex bison elfutils-devel elfutils-libelf-devel dwarves openssl openssl-devel perl
}

fedora-kernel-fetch-sources() {
    vm-command "(set -x -e
      echo root >> /etc/pesign/users
      echo $(vm-ssh-user) >> /etc/pesign/users
      # /usr/libexec/pesign/pesign-authorize
      fedpkg clone --depth 1 --anonymous kernel linux
      cd linux
      git fetch
      distro_branch='source /etc/os-release; echo \${PLATFORM_ID#*:}'
      git fetch origin refs/heads/$distro_branch
      git checkout -b $distro_branch FETCH_HEAD
      sed -i 's/# define buildid .local/%define buildid .e2e/g' kernel.spec
    )" || {
        echo "installing kernel development environment failed"
        return 1
    }
    echo "Kernel ready for patching and configuring."
    echo "build:   cd linux && dnf builddep -y kernel.spec && fedpkg local"
    echo "install: cd linux/x86_64 && dnf install -y --nogpgcheck kernel-{core-,modules-,}[5-9]*.e2e.fc*.x86_64.rpm"
}

fedora-set-kernel-cmdline() {
    local e2e_defaults="$*"
    vm-command "mkdir -p /etc/default; touch /etc/default/grub; sed -i '/e2e:fedora-set-kernel-cmdline/d' /etc/default/grub"
    vm-command "echo 'GRUB_CMDLINE_LINUX_DEFAULT=\"\${GRUB_CMDLINE_LINUX_DEFAULT} ${e2e_defaults}\" # by e2e:fedora-set-kernel-cmdline' >> /etc/default/grub" || {
        command-error "writing new command line parameters failed"
    }
    vm-command "grub2-mkconfig -o /boot/grub2/grub.cfg" || {
        command-error "updating grub failed"
    }
}
