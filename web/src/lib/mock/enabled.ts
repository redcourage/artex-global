// Mock 설정. 빌드 시 주입되는 공개 변수(NEXT_PUBLIC_ 접두사가 있어야 브라우저에서 읽을 수 있음).
// Vercel에서 NEXT_PUBLIC_MOCK=1로 설정하면 전체 사이트가 mock을 사용해 백엔드가 필요 없다.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
