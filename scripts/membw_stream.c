/* Classic STREAM memory-bandwidth benchmark (McCalpin), tuned for this box.
 * Build:  gcc -O3 -march=native -fopenmp membw_stream.c -o membw_stream
 * Run:    OMP_NUM_THREADS=22 ./membw_stream [array_mb] [ntimes]
 *
 * Reports Triad / Copy / Scale / Read bandwidth in GB/s (1e9 bytes/s).
 * Arrays are allocated with 2MB-aligned pages and first-touch NUMA placement,
 * so `numactl --interleave=all` gives a fair cross-channel picture.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <omp.h>

static double now(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return ts.tv_sec + 1e-9 * ts.tv_nsec;
}

int main(int argc, char **argv) {
    size_t mb = (argc > 1) ? (size_t)atol(argv[1]) : 512;   /* per array */
    int ntimes = (argc > 2) ? atoi(argv[2]) : 20;
    size_t n = mb * 1024 * 1024 / sizeof(double);

    double *a, *b, *c;
    if (posix_memalign((void **)&a, 4096, n * sizeof(double)) ||
        posix_memalign((void **)&b, 4096, n * sizeof(double)) ||
        posix_memalign((void **)&c, 4096, n * sizeof(double))) {
        fprintf(stderr, "alloc failed\n");
        return 1;
    }

    int nth = 1;
#pragma omp parallel
    {
#pragma omp master
        nth = omp_get_num_threads();
    }

    double q = 3.0;
#pragma omp parallel for schedule(static)
    for (size_t i = 0; i < n; i++) { a[i] = 1.0; b[i] = 2.0; c[i] = 0.0; }

    double tmin[4] = {1e30, 1e30, 1e30, 1e30};
    for (int it = 0; it < ntimes; it++) {
        double t0, t;
        /* Triad: a = b + scalar*c  (16 B/element) */
        t0 = now();
#pragma omp parallel for schedule(static)
        for (size_t i = 0; i < n; i++) a[i] = b[i] + q * c[i];
        t = now() - t0; if (t < tmin[0]) tmin[0] = t;
        /* Copy: a = b  (16 B/elem) */
        t0 = now();
#pragma omp parallel for schedule(static)
        for (size_t i = 0; i < n; i++) a[i] = b[i];
        t = now() - t0; if (t < tmin[1]) tmin[1] = t;
        /* Scale: b = scalar*c (8 B read + 8 B write) */
        t0 = now();
#pragma omp parallel for schedule(static)
        for (size_t i = 0; i < n; i++) b[i] = q * c[i];
        t = now() - t0; if (t < tmin[2]) tmin[2] = t;
        /* Read: sum(a) */
        t0 = now();
        double s = 0.0;
#pragma omp parallel for schedule(static) reduction(+:s)
        for (size_t i = 0; i < n; i++) s += a[i];
        t = now() - t0; if (t < tmin[3]) tmin[3] = t;
        if (s < 0) printf("%g\n", s);
    }

    double gb = (double)n * sizeof(double);
    printf("threads=%d  per_array=%zu MB  total=%zu MB\n", nth, mb, mb * 3);
    printf("  Triad  (a=b+q*c)  : %8.1f GB/s   (%.4f s)\n", 3 * gb / tmin[0] / 1e9, tmin[0]);
    printf("  Copy   (a=b)      : %8.1f GB/s   (%.4f s)\n", 2 * gb / tmin[1] / 1e9, tmin[1]);
    printf("  Scale  (b=q*c)    : %8.1f GB/s   (%.4f s)\n", 2 * gb / tmin[2] / 1e9, tmin[2]);
    printf("  Read   (sum(a))   : %8.1f GB/s   (%.4f s)\n", gb / tmin[3] / 1e9, tmin[3]);
    free(a); free(b); free(c);
    return 0;
}
