FROM fedora:latest as build
ENV VERIFY_CHECKSUM=false
RUN curl -fsSL -o get_helm.sh https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 \
 && chmod 700 get_helm.sh && ./get_helm.sh

FROM quay.io/operator-framework/ansible-operator:v1.32.0
COPY --from=build /usr/local/bin/helm /usr/local/bin/helm
COPY requirements.yml ${HOME}/requirements.yml
RUN ansible-galaxy collection install -r ${HOME}/requirements.yml \
 && chmod -R ug+rwx ${HOME}/.ansible

COPY watches.yaml ${HOME}/watches.yaml
COPY ansible.cfg /etc/ansible/ansible.cfg
COPY roles/ ${HOME}/roles/
COPY playbooks/ ${HOME}/playbooks/
