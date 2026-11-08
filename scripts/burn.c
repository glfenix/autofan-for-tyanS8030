// Прогрев CPU: AVX2 FMA с зависимыми цепочками, почти без обращений к памяти.
// Цель - выжать максимум тепла из каждого ядра, а не пропускную способность.
// Сборка: gcc -O3 -march=native -mfma -pthread -o burn scripts/burn.c
// Запуск:  ./burn <потоков> <секунд>     (потоков = все логические CPU хоста)
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>
#include <immintrin.h>

static volatile int stop_flag = 0;

static void *worker(void *arg) {
    (void)arg;
    __m256d a = _mm256_set1_pd(1.0000001), b = _mm256_set1_pd(0.9999999);
    __m256d c = _mm256_set1_pd(1.0000002), d = _mm256_set1_pd(1.0000003);
    __m256d x0 = _mm256_set1_pd(1.1), x1 = _mm256_set1_pd(1.2);
    __m256d x2 = _mm256_set1_pd(1.3), x3 = _mm256_set1_pd(1.4);
    __m256d x4 = _mm256_set1_pd(1.5), x5 = _mm256_set1_pd(1.6);
    __m256d x6 = _mm256_set1_pd(1.7), x7 = _mm256_set1_pd(1.8);
    while (!stop_flag) {
        for (int i = 0; i < 20000; i++) {
            x0 = _mm256_fmadd_pd(x0, a, b);
            x1 = _mm256_fmadd_pd(x1, b, c);
            x2 = _mm256_fmadd_pd(x2, c, d);
            x3 = _mm256_fmadd_pd(x3, d, a);
            x4 = _mm256_fmadd_pd(x4, a, x0);
            x5 = _mm256_fmadd_pd(x5, b, x1);
            x6 = _mm256_fmadd_pd(x6, c, x2);
            x7 = _mm256_fmadd_pd(x7, d, x3);
        }
    }
    return NULL;
}

int main(int argc, char **argv) {
    int n = argc > 1 ? atoi(argv[1]) : 24;
    int secs = argc > 2 ? atoi(argv[2]) : 60;
    pthread_t t[256];
    for (int i = 0; i < n; i++) pthread_create(&t[i], NULL, worker, NULL);
    struct timespec ts = {secs, 0};
    nanosleep(&ts, NULL);
    stop_flag = 1;
    for (int i = 0; i < n; i++) pthread_join(t[i], NULL);
    printf("done %d threads %d s\n", n, secs);
    return 0;
}
