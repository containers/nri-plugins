#!/usr/bin/env bash

# This will create and start the runner in the container and
# wait for connections. After the run, the container is removed
# and then re-created to serve new requests.

# Set the PREFIX to tell where the container mounted directories
# and other downloaded files are located.
PREFIX=${PREFIX:-`pwd`/shr}

if [ $(readlink -f "$PREFIX") = `pwd` ]; then
    echo "PREFIX $PREFIX cannot point to current directory."
    exit 1
fi

STOPPED=0
trap ctrl_c INT TERM

ctrl_c() {
    STOPPED=1
}

debug() {
    if [ -z "$DEBUG" ]; then
	return
    fi

    printf "$*\n" > /dev/tty
}

build_jwt_payload() {
        jq -c --arg iat_str "$(date +%s)" --arg app_id "${app_id}" \
        '
        ($iat_str | tonumber) as $iat
        | .iat = $iat
        | .exp = ($iat + 300)
        | .iss = ($app_id | tonumber)
        ' <<< "${payload_template}" | tr -d '\n'
}

b64enc() {
    openssl enc -base64 -A | tr '+/' '-_' | tr -d '='
}

json() {
    jq -c . | LC_CTYPE=C tr -d '\n'
}

rs256_sign() {
    openssl dgst -binary -sha256 -sign <(printf '%s\n' "$1")
}

# All the configuration data is stored in env file.
. ./env

mkdir -p "${PREFIX}/mnt"
if [ $? -ne 0 ]; then
    echo "Cannot create $PREFIX/mnt directory!"
    exit 1
fi

echo "Work directory set to $PREFIX"

get_runner_token() {
    local payload sig access_token app_private_key

    registration_url="https://api.github.com/repos/${GH_REPO}/actions/runners/registration-token"
    app_id=${GH_APP_ID}
    app_private_key="$(< ${GH_APP_PRIVATE_KEY_FILE})"
    payload_template='{}'
    header='{"alg": "RS256","typ": "JWT"}'

    payload=$(build_jwt_payload) || return
    signed_content="$(json <<<"$header" | b64enc).$(json <<<"$payload" | b64enc)"
    sig=$(printf %s "$signed_content" | rs256_sign "$app_private_key" | b64enc)

    generated_jwt="${signed_content}.${sig}"
    app_installations_url="https://api.github.com/app/installations"
    app_installations_response=$(curl -sX GET -H "Authorization: Bearer  ${generated_jwt}" -H "Accept: application/vnd.github.v3+json" ${app_installations_url})
    access_token_url=$(echo $app_installations_response | jq '.[] | select (.app_id  == '${app_id}') .access_tokens_url' --raw-output)
    access_token_response=$(curl -sX POST -H "Authorization: Bearer  ${generated_jwt}" -H "Accept: application/vnd.github.v3+json" ${access_token_url})
    access_token=$(echo $access_token_response | jq .token --raw-output)
    payload=$(curl -sX POST -H "Authorization: Bearer  ${access_token}" -H "Accept: application/vnd.github.v3+json" ${registration_url})

    debug "GH application id : '${GH_APP_ID}'"
    debug "GH private key    : '${GH_APP_PRIVATE_KEY_FILE}'"
    debug "GH registration URL      : '${registration_url}'"
    debug "GH app installations URL : '${app_installations_url}'"
    debug "GH app installations response : '${app_installations_response}'"
    debug "GH access token URL : '${access_token_url}'"
    debug "GH access token response : '${access_token_response}'"
    debug "GH access token : '${access_token}'"
    debug "GH reponse payload : $payload"

    echo $(echo $payload | jq .token --raw-output)
}

create_actions_runner() {
    mkdir -p "${PREFIX}/mnt/actions-runner"
    tar fxzC "${PREFIX}/actions-runner.tar.gz" "${PREFIX}/mnt/actions-runner"

    # Configure the runner outside of container (so that we do not have to pass
    # credentials to the container).
    (cd ${PREFIX}/mnt/actions-runner;
     ./config.sh --replace --unattended --ephemeral \
		 --name "$GH_RUNNER_NAME" \
		 --url "$GH_RUNNER_URL" \
		 --token "$GH_RUNNER_TOKEN" | \
	 tee /dev/tty | egrep -q -e "Http response code: NotFound from " -e "Invalid configuration provided for url"
     if [ $? -eq 0 ]; then
	 echo "Action runner configuration failed. Fix things and retry."
	 return 2
     fi
    )
}

remove_actions_runner() {
    # Remove the runner after we have finished working with it
    (cd ${PREFIX}/mnt/actions-runner;
     ./config.sh remove --token "${GH_RUNNER_TOKEN}"
    )

    rm -rf "${PREFIX}/mnt/actions-runner"
}

get_latest_runner() {
    LATEST_RUNNER="${PREFIX}/.github_self_hosted_runner_version"

    latest_runner_version=$(curl -I -v -s https://github.com/actions/runner/releases/latest 2>&1 | sed -n 's/^< location: \(.*\)$/\1/p' | awk -F/ '{ print $NF }' | sed 's/v//' | tr -d '\r')

    if [ -e "$LATEST_RUNNER" ]; then
	if [ -s "${PREFIX}/actions-runner.tar.gz" ]; then
	    github_self_hosted_runner_version=$(cat $LATEST_RUNNER)
	    if [ "$github_self_hosted_runner_version" == "$latest_runner_version" ]; then
		return
	    fi

	    if [ ! -z "$github_self_hosted_runner_version" ]; then
		echo "Current runner version ($github_self_hosted_runner_version) is not up to date."
	    fi

	    remove_actions_runner
	fi
    fi

    echo "Downloading latest runner version ($latest_runner_version)"

    curl --progress-bar -L "https://github.com/actions/runner/releases/download/v${latest_runner_version}/actions-runner-linux-x64-${latest_runner_version}.tar.gz" > "${PREFIX}/actions-runner.tar.gz"

    echo $latest_runner_version > "$LATEST_RUNNER"
}

start_docker_container() {
    echo "Starting container"

    # Execute the GH self hosted runner inside the container.
    docker container run --pull never \
	   -v "${PREFIX}/mnt/actions-runner:/mnt/actions-runner" \
	   -v "${PREFIX}/mnt/vagrant:/mnt/vagrant:ro" \
	   -v "${PREFIX}/mnt/env:/mnt/env:ro" \
	   -v "/var/run/docker.sock:/var/run/docker.sock" \
	   --device=/dev/kvm \
	   --env-file "${PREFIX}/mnt/env" \
	   shr-base-image \
	   /shr/runner-in-container.sh "`id --group`" "`id --user`" \
	   "`getent group docker | cut -d: -f3`" \
	   "`getent group kvm | cut -d: -f3`"
}

while [ $STOPPED -eq 0 ]; do
    GH_RUNNER_TOKEN=$(get_runner_token)
    if [ "$GH_RUNNER_TOKEN" == "null" ]; then
	echo "Cannot get self-hosted runner registration token!"
	break
    fi

    get_latest_runner

    create_actions_runner

    # Generate env file that describes local configuration data and which
    # is exported to the container.
    echo dns_nameserver="$DNS_NAMESERVER" > ${PREFIX}/mnt/env
    echo dns_search_domain="$DNS_SEARCH_DOMAIN" >> ${PREFIX}/mnt/env
    echo HTTP_PROXY="$HTTP_PROXY" >> ${PREFIX}/mnt/env
    echo HTTPS_PROXY="$HTTPS_PROXY" >> ${PREFIX}/mnt/env
    echo NO_PROXY="$NO_PROXY" >> ${PREFIX}/mnt/env
    echo http_proxy="$http_proxy" >> ${PREFIX}/mnt/env
    echo https_proxy="$https_proxy" >> ${PREFIX}/mnt/env
    echo no_proxy="$no_proxy" >> ${PREFIX}/mnt/env

    (
	start_docker_container
	if [ $? -ne 0 ]; then
	    # User stopped the script, we should quit
	    STOPPED=1
	    exit 1
	fi
    ) &

    wait -f

    remove_actions_runner

    unset GH_RUNNER_NAME GH_RUNNER_URL GH_RUNNER_TOKEN

    # Re-read the values if user has changed them.
    . ./env
done
