// Copyright The NRI Plugins Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// sleep-accuracy - Measure the accuracy of nanosleep under various conditions.

/*
  Debug tips:

  CPU affinity and toggling can be observed with:

    SLEEP_PID=$(pgrep sleep-accuracy | sort -n | head -n 1)
    sudo bpftrace -e "tracepoint:sched:sched_stat_runtime{ if(args->pid == $SLEEP_PID) { @run[cpu]+=args->runtime } } interval:ms:100{ print(@run);  }"
*/

#define _GNU_SOURCE

#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <time.h>
#include <unistd.h>
#include <signal.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <sys/epoll.h>

#define uint64_t u_int64_t

#define NS_PER_SEC 1000000000ULL
#define MICROSECOND 1000ULL
#define MILLISECOND 1000000ULL

// MAX_COMB - maximum number of combinations for cpus, pol/prio, busy, sleep...
#define MAX_COMB 10

pid_t main_thread_pid = 0;

typedef enum {
    BENCHMARK_NANOSLEEP,
    BENCHMARK_NETWORKING,
} benchmark_type_t;

// options
typedef struct {
    int cpus[MAX_COMB][2];           // CPUs to pin or toggle
    int cpu_count;                   // Number of CPU cores
    int polprio[MAX_COMB][2];        // Scheduling policy and priority pairs
    int polprio_count;               // Number of policy/priority pairs
    int cpuidle_minmax[MAX_COMB][2]; // cpuidle min/max state pairs
    int cpuidle_count;               // Number of cpuidle min/max pairs
    int cpufreq_minmax[MAX_COMB][2]; // cpufreq min/max [kHz] pairs
    int cpufreq_count;               // Number of cpufreq min/max pairs
    int64_t busy_times[MAX_COMB];    // Busy durations in nanoseconds
    int busy_count;                  // Number of busy durations
    int64_t sleep_times[MAX_COMB];   // Sleep durations in nanoseconds
    int sleep_count;                 // Number of sleep durations
    int64_t toggle_intervals[MAX_COMB]; // CPU toggling intervals [ns]
    int toggle_count;                // Number of CPU toggling intervals
    int64_t iterations;              // Number of iterations per measurement
    int repeats;                     // Number of repetitions for each measurement
    benchmark_type_t benchmarks[MAX_COMB]; // Benchmarks to run
    int benchmark_count;             // Number of benchmarks
} options_t;

options_t options = {};

void print_usage() {
    printf(
        "sleep-accuracy - Measure the accuracy of nanosleep under various conditions.\n"
        "\n"
        "Usage: sleep-accuracy [options]\n"
        "Options:\n"
        "  -c <cpu,...>       Comma-separated list of CPUs to pin one at a time (default: no pinning)\n"
        "  -c <cpu0/cpu1,...> Comma-separated list of CPUs where affinity is toggled one at a time (see -t)\n"
        "  -t <interval,...>  Comma-separated list of CPU toggling intervals [ns], if CPU toggling is used with -c cpu0/cpu1 (default: 1000000)\n"
        "  -p <pol/prio,...>  Comma-separated list of Scheduling policy/priority.\n"
        "                     0=OTHER, 1=FIFO, 2=RR, 3=BATCH, 5=IDLE (default: 0/0), see sched_setscheduler(2)\n"
        "  -f <min/max,...>   Comma-separated list of cpufreq min/max [kHz] pairs (default: 0/9999999)\n"
        "  -i <min/max,...>   Comma-separated list of cpuidle min/max state pairs (default: 0/99)\n"
        "  -b <benchmarks>    Comma-separated list of benchmarks to run: nanosleep,networking (default: nanosleep)\n"
        "  -B <busy,...>      Comma-separated list of busy durations [ns] (default: 0,1000,1000000)\n"
        "  -s <sleep,...>     Comma-separated list of sleep durations [ns] (default: 0,1000,1000000)\n"
        "  -r <repeats>       Number of repetitions for each measurement (default: 1)\n"
        "  -I <iterations>    Number of iterations per measurement (default: 1000)\n"
        "  -h                 Show this help message\n"
        "\n"
        "Example:\n"
        "  sleep-accuracy -c 3/13,3,13 -t 1000000,100000 -p 0/0,1/1 -f 1200000/1200000,0/9999999 -i -1/-1,0/1,0/9 -B 20000 -s 50000 -I 10000 -r 5\n"
        "    report requested sleep accuracy when...\n"
        "    -c 3/13,3,13: migrating between CPUs 3 and 13 or running only on CPU 3 or 13\n"
        "    -t 1000000,10000: ...migrating every 1 ms or 100 us,\n"
        "    -p 0/0,1/1: ...with SCHED_OTHER prio0 or SCHED_FIFO prio1,\n"
        "    -f 1200000/1200000,0/9999999: ...with CPU(s) fixed at 1.2 GHz or platforms min/max frequencies,\n"
        "    -i -1/-1,0/1,0/9: ...with no states, only states 0 and 1, or all idle states enabled\n"
        "    -B 20000: ...running busy for 20us before each sleep,\n"
        "    -s 50000: ...requesting 50us sleep,\n"
        "    -I 10000: ...repeating each measurement 10k times to get statistically significant results,\n"
        "    -r 5: ...and repeating the whole measurement 5 times to see variation between runs.\n"
    );
}

// delay - sleep for specified nanoseconds
void delay(uint64_t ns) {
    struct timespec req, rem;
    req.tv_sec = ns / NS_PER_SEC;
    req.tv_nsec = ns % NS_PER_SEC;
    while (nanosleep(&req, &rem) == -1) {
        req = rem; // continue sleeping for the remaining time if interrupted
    }
}

// set_cpu_affinity - set CPU affinity of the main thread to a specific CPU
void set_cpu_affinity(int cpu) {
  cpu_set_t cpuset;
  CPU_ZERO(&cpuset);
  CPU_SET(cpu, &cpuset);
  if (sched_setaffinity(main_thread_pid, sizeof(cpuset), &cpuset) == -1) {
    perror("sched_setaffinity");
    exit(EXIT_FAILURE);
  }
}

// set_scheduler - set scheduling policy and priority
void set_scheduler(int policy, int priority) {
    struct sched_param param;
    param.sched_priority = priority;
    if (sched_setscheduler(0, policy, &param) == -1) {
        perror("sched_setscheduler");
        exit(EXIT_FAILURE);
    }
}

// set_cpuidle_minmax - enable/disable cpuidle/stateX's
void set_cpuidle_minmax(int cpu, int min, int max) {
    char disable_filename[1024];
    int state = 0;
    FILE *f = NULL;
    while (1) {
        sprintf(disable_filename, "/sys/devices/system/cpu/cpu%d/cpuidle/state%d/disable", cpu, state);
        FILE *f = fopen(disable_filename, "w");
        if (!f) {
            if (state == 0 && max != 99) {
                perror("cannot open for writing: cpuidle/state0/disable");
            }
            break; // all cpuidle states processed
        }
        fprintf(f, "%d\n", (state < min || state > max) ? 1 : 0);
        fflush(f);
        fsync(fileno(f));
        fclose(f);
        state++;
    }
    if (f) fclose(f);
}

// get_cpuidle_minmax - read min and max cpuidle states for the CPU from sysfs
void get_cpuidle_minmax(int cpu, int *min, int *max) {
    char disable_filename[1024];
    int state = 0;
    FILE *f = NULL;
    *min = -1;
    *max = -1;
    while (1) {
        sprintf(disable_filename, "/sys/devices/system/cpu/cpu%d/cpuidle/state%d/disable", cpu, state);
        f = fopen(disable_filename, "r");
        if (!f) {
            break; // all cpuidle states processed
        }
        int disabled = 0;
        fscanf(f, "%d", &disabled);
        fclose(f);
        if (!disabled) {
            if (*min == -1) *min = state;
            *max = state;
        }
        state++;
    }
}

// set_cpufreq_minmax - set min and max cpufreq for the CPU in sysfs
void set_cpufreq_minmax(int cpu, int min, int max) {
    char freq_filename[1024];
    FILE *f = NULL;

    sprintf(freq_filename, "/sys/devices/system/cpu/cpu%d/cpufreq/scaling_max_freq", cpu);
    f = fopen(freq_filename, "w");
    if (f) {
        fprintf(f, "%d\n", max);
        fflush(f);
        fsync(fileno(f));
        fclose(f);
    } else {
        perror("cannot open for writing: cpufreq/scaling_max_freq");
    }

    sprintf(freq_filename, "/sys/devices/system/cpu/cpu%d/cpufreq/scaling_min_freq", cpu);
    f = fopen(freq_filename, "w");
    if (f) {
        fprintf(f, "%d\n", min);
        fflush(f);
        fsync(fileno(f));
        fclose(f);
    } else {
        perror("cannot open for writing: cpufreq/scaling_min_freq");
    }
}

// get_cpufreq_minmax - read min and max cpufreq for the CPU from sysfs
void get_cpufreq_minmax(int cpu, int *min, int *max) {
    char freq_filename[1024];
    FILE *f = NULL;

    sprintf(freq_filename, "/sys/devices/system/cpu/cpu%d/cpufreq/scaling_max_freq", cpu);
    f = fopen(freq_filename, "r");
    if (f) {
        fscanf(f, "%d", max);
        fclose(f);
    } else {
        perror("cannot open for reading: cpufreq/scaling_max_freq");
    }

    sprintf(freq_filename, "/sys/devices/system/cpu/cpu%d/cpufreq/scaling_min_freq", cpu);
    f = fopen(freq_filename, "r");
    if (f) {
        fscanf(f, "%d", min);
        fclose(f);
    } else {
        perror("cannot open for reading: cpufreq/scaling_min_freq");
    }
}

// get_time_ns - get current time in nanoseconds
uint64_t get_time_ns() {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return (uint64_t)ts.tv_sec * NS_PER_SEC + (uint64_t)ts.tv_nsec;
}

// compare_uint64 - comparison function for qsort
int compare_uint64(const void *a, const void *b) {
  uint64_t val1 = *(const uint64_t *)a;
  uint64_t val2 = *(const uint64_t *)b;
  if (val1 < val2) return -1;
  if (val1 > val2) return 1;
  return 0;
}

// busy-wait for a specified duration
void busy_wait(uint64_t duration_ns) {
    u_int64_t start = get_time_ns();
    while (get_time_ns() - start < duration_ns);
}

// measure_nanosleep - perform measurements (all iterations) of nanosleep latency
void measure_nanosleep(int64_t busy_ns, int64_t sleep_ns, int64_t *out_latencies) {
    int64_t iters = options.iterations;

    for (int i = 0; i < iters; i++) {
        if (busy_ns > 0) {
            busy_wait(busy_ns);  // Simulate work before sleep
        }
        int64_t sleep_start = get_time_ns();

        // request a short sleep using nanosleep, even if sleep_ns is 0
        if (sleep_ns >= 0) {
            struct timespec req = {0, sleep_ns};
            nanosleep(&req, NULL);
        }

        int64_t sleep_end = get_time_ns();
        int64_t actual_sleep = sleep_end - sleep_start;
        int64_t latency = actual_sleep - sleep_ns;

        out_latencies[i] = latency;
    }
}

// measure_networking - measure networking latency using loopback socket communication
void measure_networking(int64_t busy_ns, int64_t sleep_ns, int64_t *out_latencies) {
    int64_t iters = options.iterations;
    int server_fd, client_fd, conn_fd;
    struct sockaddr_in server_addr, client_addr;
    socklen_t addr_len = sizeof(client_addr);
    char buffer[1];

    // Create server socket
    server_fd = socket(AF_INET, SOCK_STREAM, 0);
    if (server_fd < 0) {
        perror("socket creation failed");
        for (int i = 0; i < iters; i++) out_latencies[i] = -1;
        return;
    }

    int opt = 1;
    setsockopt(server_fd, SOL_SOCKET, SO_REUSEADDR | SO_REUSEPORT, &opt, sizeof(opt));

    memset(&server_addr, 0, sizeof(server_addr));
    server_addr.sin_family = AF_INET;
    server_addr.sin_addr.s_addr = inet_addr("127.0.0.1");
    server_addr.sin_port = 0; // Let OS assign a port

    if (bind(server_fd, (struct sockaddr *)&server_addr, sizeof(server_addr)) < 0) {
        perror("bind failed");
        close(server_fd);
        for (int i = 0; i < iters; i++) out_latencies[i] = -1;
        return;
    }

    if (listen(server_fd, 1) < 0) {
        perror("listen failed");
        close(server_fd);
        for (int i = 0; i < iters; i++) out_latencies[i] = -1;
        return;
    }

    // Get the assigned port
    addr_len = sizeof(server_addr);
    getsockname(server_fd, (struct sockaddr *)&server_addr, &addr_len);

    // Create client socket
    client_fd = socket(AF_INET, SOCK_STREAM, 0);
    if (client_fd < 0) {
        perror("client socket creation failed");
        close(server_fd);
        for (int i = 0; i < iters; i++) out_latencies[i] = -1;
        return;
    }

    // Set non-blocking for connect to avoid blocking
    fcntl(client_fd, F_SETFL, O_NONBLOCK);

    // Connect to server
    connect(client_fd, (struct sockaddr *)&server_addr, sizeof(server_addr));

    // Accept connection
    conn_fd = accept(server_fd, (struct sockaddr *)&client_addr, &addr_len);
    if (conn_fd < 0) {
        perror("accept failed");
        close(client_fd);
        close(server_fd);
        for (int i = 0; i < iters; i++) out_latencies[i] = -1;
        return;
    }

    // Set sockets back to blocking mode
    fcntl(client_fd, F_SETFL, fcntl(client_fd, F_GETFL) & ~O_NONBLOCK);
    fcntl(conn_fd, F_SETFL, fcntl(conn_fd, F_GETFL) & ~O_NONBLOCK);

    // Measure round-trip latency
    for (int i = 0; i < iters; i++) {
        if (busy_ns > 0) {
            busy_wait(busy_ns);
        }

        int64_t start = get_time_ns();

        // Send one byte
        buffer[0] = 'x';
        if (send(client_fd, buffer, 1, 0) < 0) {
            out_latencies[i] = -1;
            continue;
        }

        // Receive echo back
        if (recv(conn_fd, buffer, 1, 0) < 0) {
            out_latencies[i] = -1;
            continue;
        }

        // Echo back to client
        if (send(conn_fd, buffer, 1, 0) < 0) {
            out_latencies[i] = -1;
            continue;
        }

        // Receive at client
        if (recv(client_fd, buffer, 1, 0) < 0) {
            out_latencies[i] = -1;
            continue;
        }

        int64_t end = get_time_ns();
        int64_t latency = (end - start) / 2; // Divide by 2 for one-way latency approximation

        out_latencies[i] = latency;
    }

    close(conn_fd);
    close(client_fd);
    close(server_fd);
}

const char* benchmark_name(benchmark_type_t type) {
    switch (type) {
        case BENCHMARK_NANOSLEEP: return "nanosleep";
        case BENCHMARK_NETWORKING: return "networking";
        default: return "unknown";
    }
}

void print_latencies(int64_t *latencies) {
    uint64_t total_latency = 0;
    int64_t iters = options.iterations;
    for (int i = 0; i < iters; i++) {
        total_latency += latencies[i];
    }

    // Sort latencies for percentile calculation
    qsort(latencies, iters, sizeof(uint64_t), compare_uint64);

    double avg_latency = (double)total_latency / iters;

    // Calculate percentiles
    int64_t min = latencies[0];
    int64_t p5 = latencies[(int)(iters * 0.05)];
    int64_t p50 = latencies[(int)(iters * 0.5)];
    int64_t p80 = latencies[(int)(iters * 0.8)];
    int64_t p90 = latencies[(int)(iters * 0.9)];
    int64_t p95 = latencies[(int)(iters * 0.95)];
    int64_t p99 = latencies[(int)(iters * 0.99)];
    int64_t p999 = latencies[(int)(iters * 0.999)];
    int64_t max = latencies[iters - 1];

    // Print results
    printf("%ld %ld %ld %ld %ld %ld %ld %ld %ld %.0f", min, p5, p50, p80, p90, p95, p99, p999, max, avg_latency);
}

void parse_options(int argc, char *argv[]) {
    // Default values
    options.cpu_count = 0;
    options.polprio_count = 0;
    options.busy_count = 0;
    options.sleep_count = 0;
    options.toggle_count = 0;
    options.benchmark_count = 0;
    options.iterations = 1000;
    options.repeats = 1;

    options.polprio[options.polprio_count][0] = 0; // Default policy OTHER
    options.polprio[options.polprio_count++][1] = 0; // Default priority 0

    options.cpuidle_minmax[options.cpuidle_count][0] = 0; // Default cpuidle min state
    options.cpuidle_minmax[options.cpuidle_count++][1] = 99; // Default cpuidle max state

    options.cpufreq_minmax[options.cpufreq_count][0] = 0; // Default cpufreq min [kHz]
    options.cpufreq_minmax[options.cpufreq_count++][1] = 9999999; // Default cpufreq max [kHz]

    options.benchmarks[options.benchmark_count++] = BENCHMARK_NANOSLEEP; // Default benchmark

    options.busy_times[options.busy_count++] = 0;
    options.busy_times[options.busy_count++] = 1000;
    options.busy_times[options.busy_count++] = 1000000;

    options.sleep_times[options.sleep_count++] = 0;
    options.sleep_times[options.sleep_count++] = 1000;
    options.sleep_times[options.sleep_count++] = 1000000;

    options.toggle_intervals[options.toggle_count++] = 1000000; // Default 1 ms

    // Parse command-line arguments
    for (int i = 1; i < argc; i++) {
        if (strcmp(argv[i], "-c") == 0 && i + 1 < argc) {
            char *token = strtok(argv[++i], ",");
            while (token && options.cpu_count < MAX_COMB) {
                char *slash = strchr(token, '/');
                if (slash) {
                    *slash = '\0';
                    options.cpus[options.cpu_count][0] = atoi(token);
                    options.cpus[options.cpu_count++][1] = atoi(slash + 1);
                } else {
                    options.cpus[options.cpu_count++][0] = atoi(token);
                    options.cpus[options.cpu_count - 1][1] = -1; // indicate single CPU pinning
                }
                token = strtok(NULL, ",");
            }
        } else if (strcmp(argv[i], "-p") == 0 && i + 1 < argc) {
            options.polprio_count = 0; // Reset defaults
            char *token = strtok(argv[++i], ",");
            while (token && options.polprio_count < MAX_COMB) {
                char *slash = strchr(token, '/');
                if (slash) {
                    *slash = '\0';
                    options.polprio[options.polprio_count][0] = atoi(token);
                    options.polprio[options.polprio_count++][1] = atoi(slash + 1);
                }
                token = strtok(NULL, ",");
            }
        } else if (strcmp(argv[i], "-i") == 0 && i + 1 < argc) {
            options.cpuidle_count = 0; // Reset defaults
            char *token = strtok(argv[++i], ",");
            while (token && options.cpuidle_count < MAX_COMB) {
                char *slash = strchr(token, '/');
                if (slash) {
                    *slash = '\0';
                    options.cpuidle_minmax[options.cpuidle_count][0] = atoi(token);
                    options.cpuidle_minmax[options.cpuidle_count++][1] = atoi(slash + 1);
                }
                token = strtok(NULL, ",");
            }
        } else if (strcmp(argv[i], "-f") == 0 && i + 1 < argc) {
            options.cpufreq_count = 0; // Reset defaults
            char *token = strtok(argv[++i], ",");
            while (token && options.cpufreq_count < MAX_COMB) {
                char *slash = strchr(token, '/');
                if (slash) {
                    *slash = '\0';
                    options.cpufreq_minmax[options.cpufreq_count][0] = atoi(token);
                    options.cpufreq_minmax[options.cpufreq_count++][1] = atoi(slash + 1);
                }
                token = strtok(NULL, ",");
            }
        } else if (strcmp(argv[i], "-b") == 0 && i + 1 < argc) {
            options.benchmark_count = 0; // Reset defaults
            char *token = strtok(argv[++i], ",");
            while (token && options.benchmark_count < MAX_COMB) {
                if (strcmp(token, "nanosleep") == 0) {
                    options.benchmarks[options.benchmark_count++] = BENCHMARK_NANOSLEEP;
                } else if (strcmp(token, "networking") == 0) {
                    options.benchmarks[options.benchmark_count++] = BENCHMARK_NETWORKING;
                } else {
                    fprintf(stderr, "Unknown benchmark: %s\n", token);
                    exit(EXIT_FAILURE);
                }
                token = strtok(NULL, ",");
            }
        } else if (strcmp(argv[i], "-B") == 0 && i + 1 < argc) {
            options.busy_count = 0; // Reset defaults
            char *token = strtok(argv[++i], ",");
            while (token && options.busy_count < MAX_COMB) {
                options.busy_times[options.busy_count++] = strtoull(token, NULL, 10);
                token = strtok(NULL, ",");
            }
        } else if (strcmp(argv[i], "-s") == 0 && i + 1 < argc) {
            options.sleep_count = 0; // Reset defaults
            char *token = strtok(argv[++i], ",");
            while (token && options.sleep_count < MAX_COMB) {
                options.sleep_times[options.sleep_count++] = strtoull(token, NULL, 10);
                token = strtok(NULL, ",");
            }
        } else if (strcmp(argv[i], "-t") == 0 && i + 1 < argc) {
            options.toggle_count = 0; // Reset defaults
            char *token = strtok(argv[++i], ",");
            while (token && options.toggle_count < MAX_COMB) {
                options.toggle_intervals[options.toggle_count++] = strtoull(token, NULL, 10);
                token = strtok(NULL, ",");
            }
        } else if (strcmp(argv[i], "-I") == 0 && i + 1 < argc) {
            options.iterations = atoi(argv[++i]);
        } else if (strcmp(argv[i], "-r") == 0 && i + 1 < argc) {
            options.repeats = atoi(argv[++i]);
        } else if (strcmp(argv[i], "-h") == 0) {
            print_usage();
            exit(0);
        } else {
            fprintf(stderr, "Unknown option: %s\n", argv[i]);
            exit(EXIT_FAILURE);
        }
    }
}

// configure_cpu_toggler - launch and/or reconfigure a thread that toggles CPU affinity between two CPUs at specified intervals
int toggle_cpu0 = -1;
int toggle_cpu1 = -1;
uint64_t toggle_cpu_interval_ns = 1000000; // default 1 ms
int toggle_cpu_running = 0;
void configure_cpu_toggler(int cpu0, int cpu1, int interval_ns) {
    toggle_cpu0 = cpu0;
    toggle_cpu1 = cpu1;
    toggle_cpu_interval_ns = interval_ns;
    if (toggle_cpu_running) return; // toggler already running
    toggle_cpu_running = 1;

    pid_t pid = fork();
    if (pid == -1) {
        perror("configure_cpu_toggler: fork failed");
        exit(EXIT_FAILURE);
    }
    if (pid != 0) {
        // Parent process - main thread
        return;
    }
    // Child process - toggler thread
    prctl(PR_SET_PDEATHSIG, SIGTERM); // ensure child exits when parent exits
    if (getppid() == 1) {
        exit(0); // main thread already exited, so do we
    }
    while (1) {
        if (toggle_cpu0 != -1) {
            set_cpu_affinity(toggle_cpu0);
            delay(toggle_cpu_interval_ns);
        }
        if (toggle_cpu1 != -1) {
            set_cpu_affinity(toggle_cpu1);
            delay(toggle_cpu_interval_ns);
        } else {
            // Wait until there is more than single CPU to toggle again.
            // This prevents keep setting main thread's CPU affinity to same CPU in a loop.
            while (toggle_cpu0 != -1) {
                delay(toggle_cpu_interval_ns);
            }
        }
    }
}

int main(int argc, char *argv[]) {
    int64_t *latencies;
    parse_options(argc, argv);

    latencies = malloc(sizeof(int64_t) * options.iterations);
    if(!latencies) {
        perror("allocating memory for latencies failed");
        exit(EXIT_FAILURE);
    }

    main_thread_pid = getpid();

    printf("benchmark round cpu0 cpu1 cpumigr_ns schedpol schedprio idlemin idlemax freqmin freqmax busy_ns sleep_ns min p5 p50 p80 p90 p95 p99 p999 max avg\n");

    for (int r = 0; r < options.repeats; r++) {

        for (int bench_idx = 0; bench_idx < options.benchmark_count; bench_idx++) {
            benchmark_type_t benchmark = options.benchmarks[bench_idx];

        for (int toggle_idx = 0; toggle_idx < options.toggle_count; toggle_idx++) {
            int toggle_ns = options.toggle_intervals[toggle_idx];

            for (int cpu_idx = 0; cpu_idx < (options.cpu_count ? options.cpu_count : 1); cpu_idx++) {
                int cpu = options.cpu_count ? options.cpus[cpu_idx][0] : -1;
                int cpu_other = options.cpu_count ? options.cpus[cpu_idx][1] : -1;
                if (cpu != -1) {
                    set_cpu_affinity(cpu);
                }
                if (cpu_other != -1 || toggle_cpu_running) {
                    configure_cpu_toggler(cpu, cpu_other, toggle_ns);
                }


                for (int pp_idx = 0; pp_idx < options.polprio_count; pp_idx++) {
                    set_scheduler(options.polprio[pp_idx][0], options.polprio[pp_idx][1]);

                    for (int cpuidle_idx = 0; cpuidle_idx < options.cpuidle_count; cpuidle_idx++) {
                        int cpuidle_min = -1;
                        int cpuidle_max = -1;
                        if (cpu != -1) {
                            cpuidle_min = options.cpuidle_minmax[cpuidle_idx][0];
                            cpuidle_max = options.cpuidle_minmax[cpuidle_idx][1];
                            set_cpuidle_minmax(cpu, cpuidle_min, cpuidle_max);
                            if (cpu_other != -1) {
                                set_cpuidle_minmax(cpu_other, cpuidle_min, cpuidle_max);
                            }
                        }

                        for (int cpufreq_idx = 0; cpufreq_idx < options.cpufreq_count; cpufreq_idx++) {
                            int cpufreq_min = -1;
                            int cpufreq_max = -1;
                            if (cpu != -1) {
                                cpufreq_min = options.cpufreq_minmax[cpufreq_idx][0];
                                cpufreq_max = options.cpufreq_minmax[cpufreq_idx][1];
                                set_cpufreq_minmax(cpu, cpufreq_min, cpufreq_max);
                                if (cpu_other != -1) {
                                    set_cpufreq_minmax(cpu_other, cpufreq_min, cpufreq_max);
                                }
                            }

                            for (int b_idx = 0; b_idx < options.busy_count; b_idx++) {

                                for (int s_idx = 0; s_idx < options.sleep_count; s_idx++) {

                                    if (toggle_cpu_running) {
                                        delay(toggle_cpu_interval_ns * 2); // give toggler some time to reconfigure CPU affinity
                                    }
                                    if (cpu != -1) {
                                        delay(10 * MILLISECOND); // give kernel some time for affinity, cpufreq and cpuidle settings to take effect
                                        get_cpufreq_minmax(cpu, &cpufreq_min, &cpufreq_max);
                                        get_cpuidle_minmax(cpu, &cpuidle_min, &cpuidle_max);
                                    }

                                    // Call the appropriate benchmark function
                                    switch (benchmark) {
                                        case BENCHMARK_NANOSLEEP:
                                            measure_nanosleep(options.busy_times[b_idx], options.sleep_times[s_idx], latencies);
                                            break;
                                        case BENCHMARK_NETWORKING:
                                            measure_networking(options.busy_times[b_idx], options.sleep_times[s_idx], latencies);
                                            break;
                                    }
                                    
                                    // print measurement parameters and results
                                    printf("%s %d %d %d %ld %d %d %d %d %d %d %ld %ld ",
                                           benchmark_name(benchmark),
                                           r + 1,
                                           cpu,
                                           cpu_other,
                                           cpu_other != -1 ? toggle_cpu_interval_ns : -1,
                                           options.polprio[pp_idx][0],
                                           options.polprio[pp_idx][1],
                                           cpuidle_min,
                                           cpuidle_max,
                                           cpufreq_min,
                                           cpufreq_max,
                                           options.busy_times[b_idx],
                                           options.sleep_times[s_idx]);
                                    print_latencies(latencies);
                                    printf("\n");
                                    fflush(stdout);
                                }
                            }

                            if (cpu != -1) set_cpufreq_minmax(cpu, 0, 9999999); // reset cpufreq
                            if (cpu_other != -1) set_cpufreq_minmax(cpu_other, 0, 9999999); // reset cpufreq
                        }

                        if (cpu != -1) set_cpuidle_minmax(cpu, 0, 99); // reset cpuidle
                        if (cpu_other != -1) set_cpuidle_minmax(cpu_other, 0, 99); // reset cpuidle
                    }
                }
            }
        }
        }
    }
}
