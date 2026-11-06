# Introduction

NRI Resource Policy is a NRI container runtime plugin. It is connected
to Container Runtime implementation (containerd, cri-o) via NRI API.
The main purpose of the the NRI resource plugin is to apply hardware-aware
resource allocation policies to the containers running in the system.

There are different policies available, each with a different set of
goals in mind and implementing different hardware allocation strategies. The
details of whether and how a container resource request is altered or
if extra actions are performed depend on which policy plugin is running
and how that policy is configured.
