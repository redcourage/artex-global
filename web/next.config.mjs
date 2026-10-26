import createNextIntlPlugin from "next-intl/plugin";

import { fileURLToPath } from "node:url";

// 정적 내보내기: `NEXT_EXPORT=1 next build`는 web/out에 순수 정적 디렉터리를 생성하며
// nginx 웹 루트에 바로 배치할 수 있다. 개발(next dev)에서는 변수를 설정하지 않아 /api 역방향 프록시와 핫 리로드를 유지한다.
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel 데모: 전체 사이트에서 mock을 사용하므로 백엔드와 /api 역방향 프록시가 필요 없다.
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // 상위 디렉터리 lockfile이 루트 추론과 리소스 경로 생성에 영향을 주지 않게 한다.
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // 로컬 네트워크 IP의 개발 리소스(HMR) 접근 허용. 필요에 따라 추가/제거한다.
  // 개발 시 모든 IPv4 출처의 /_next/* 및 HMR 접근 허용(로컬 IP 변경에도 유지).
  // Next는 보안상 단독 "*"를 금지하므로 구간별 와일드카드 사용. "*.*.*.*"는 모든 IPv4에 일치한다.
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // 순수 정적 내보내기: Node 런타임 없음, 이미지 최적화 없음, 각 라우트에 <route>/index.html 생성.
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock 데모: 백엔드가 없어 /api 역방향 프록시가 필요 없다.
          images: { unoptimized: true },
        }
      : {
          // 개발: /api/*를 Go 백엔드로 프록시(기본 :8787, AUTOPENTEST_API로 재정의 가능).
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

// next-intl 플러그인. 요청 설정은 src/i18n/request.ts 에 둔다. i18n 경로 라우팅을
// 쓰지 않으므로 미들웨어는 추가하지 않는다(정적 내보내기 output: "export" 와 호환).
const withNextIntl = createNextIntlPlugin("./src/i18n/request.ts");

export default withNextIntl(nextConfig);
