import functools


FACTORIAL = [1]
for i in range(1, 200):
    FACTORIAL.append(FACTORIAL[i - 1] * i)


def factorial(n):
    return FACTORIAL[n]
    # r = 1
    # for i in range(n):
    #     r *= i + 1
    # return r


assert factorial(5) == 5 * 4 * 3 * 2


@functools.lru_cache(maxsize=None)
def comb(m, n):
    return factorial(m) / factorial(m - n) / factorial(n)


def p_dist(m, n, miss1, miss2):
    assert miss1 <= m and miss2 <= n
    return comb(m, miss1) * comb(n, miss2) / comb(m + n, miss1 + miss2)


assert p_dist(1, 1, 1, 1) == 1


@functools.lru_cache(maxsize=None)
def n_loss(m, n, miss):
    assert miss > n
    e = 0
    for miss1 in range(1, miss + 1):
        miss2 = miss - miss1
        if not (miss1 <= m and 0 <= miss2 <= n):
            continue
        e += miss1 * p_dist(m, n, miss1, miss2)
    return e


assert n_loss(1, 1, 2) == 1


def loss_rate(p, m, n):
    rate = 0
    for miss in range(n + 1, m + n + 1):
        p_miss = p ** miss * (1 - p) ** (m + n - miss) * comb(m + n, miss)
        rate += p_miss * n_loss(m, n, miss) / m
    assert rate <= 1
    return rate


def table(p):
    for m in range(1, 20):
        for n in range(1, m + 1):
            if n / (m + n) < p:
                continue
            loss = loss_rate(p, m, n)
            print('p=%.2f m=%02d n=%02d loss=%.03g' % (p, m, n, loss))
        print()
    print()


# var gParityMap [kRSSenderLossPercentMax][kRSSenderMaxDataShard]uint8
def threshold(p, max_loss):
    for m in range(1, kRSSenderMaxDataShard + 1):
        for n in range(1, 5 * m):
            if n / (m + n) < p:
                continue
            loss = loss_rate(p, m, n)
            waste = n / (m + n)
            is_last = n == 5 * m - 1 or m + n >= 200
            if loss < max_loss or is_last:
                # print('p=%.2f m=%02d n=%02d waste=%.2f loss=%.03g' % (p, m, n, waste, loss))
                print(f'\tgParityMap[{int(p * 100) - 1}][{m - 1}] = {n} // p={p:.02f} m={m:02d} n={n:02d} waste={waste:.03f} loss={loss:.03g}')
                break
    print()


kRSSenderLossPercentMax = 50
kRSSenderMaxDataShard = 40

print('''package single_udp_tun

func init() {''')
for p in range(1, kRSSenderLossPercentMax + 1, 1):
    p /= 100
    # table(p)
    threshold(p, 0.01)
print('}')
