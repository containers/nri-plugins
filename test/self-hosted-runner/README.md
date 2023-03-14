Github self hosted action runner setup
======================================

This package describes how to setup Github action-runner [[1]](#s1) to run in
self hosted environment [[2]](#s2).

The setup is devided into two parts, in step 1) a generic Ubuntu 22.04 image
is used as a base and new software like Go compiler etc. is installed into it.
Then in step 2) this base image is used to run the actual e2e tests in it.
A freshly installed Ubuntu image is used for each e2e test run.

There are several layer of VMs and host OS here:

* Your host OS can be Fedora or some other Linux distribution. In the examples
  below, the host installation instructions are for Fedora but you can use
  Ubuntu too.

* In the host OS, we will install Docker tool that manages the action runner
  container. The base OS in container is Ubuntu 22.04. If you want to use some
  other OS, create a new Dockerfile for the OS and place it into docker/
  directory.

* The actual e2e tests are run inside a Vagrant created VM inside the Docker
  container. The the e2e scripts use ansible tool to install the actual VM
  image used in the testing. The desired e2e OS is selected by the github
  workflow file and is not selected by the action runner scripts.

Note that the self hosted runner needs to be configured in Github side first.
You need to create a Github App that is able to create runner registration
token needed by the actual runner script.

Here is an overview of the entire process for creating a self-hosted
registration token using an App:

* Create a GitHub App with the "Repository permissions" set to
  “Administration: Read & Write”. [[3]](#s3)

* Create a private key file (PEM) for that and download it. [[4]](#s4)

* Install the Github App onto your account and desired repository.

This will allow the `runner.sh` script to create an access token which can
then create the self-hosted registration token needed to run the e2e tests.

Details
-------

The base container image is created in your host where you need to install
Docker.

For Fedora, this means you need to install these packages to your host:

```
   $ sudo dnf install docker make
```

For Ubuntu, you need to install these packages to your host:

```
   $ sudo apt install make
   $ sudo sh -c "curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg"
   $ sudo sh -c 'echo "deb [arch=amd64 signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" | tee /etc/apt/sources.list.d/docker.list > /dev/null'
   $ sudo sh -c "apt-get update -y && apt-get install -y docker-ce docker-ce-cli containerd.io"
```

Create a configuration file for the runner:

```
   $ make create-env
```

Edit the generated env file, and add relevant parameters from github action
runner settings there. If you are behind a proxy, modify the proxy settings
in env file accordingly.

Final step is to configure and run the self-hosted action runner.
Note that at this point, the runner will contact github so the Github
configuration settings in the env configuration file needs to be set properly.

```
   $ make run
```

The `make run` will launch `runner.sh` which will create the runner container
and execute the actual self-hosted runner script (`run.sh`) that is provided
by the github action runner source package. The `run.sh` script will then wait
for jobs from the repository you have configured.

When a runner job has finished i.e., the e2e tests have been executed,
the results can be seen in github actions page. The self-hosted action runner
container in destroyed after the job, and new and fresh container is created
to be ready to serve new job requests.

[1]<a name="s1"></a> https://docs.github.com/en/rest/actions

[2]<a name="s2"></a> https://docs.github.com/en/rest/actions/self-hosted-runners

[3]<a name="s3"></a> https://docs.github.com/en/developers/apps/building-github-apps/creating-a-github-app

[4]<a name="s4"></a> https://docs.github.com/en/developers/apps/authenticating-with-github-apps#generating-a-private-key
