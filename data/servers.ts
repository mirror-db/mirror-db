import { ServerTag } from "./servers-tags";

export class Server {
  id: string = "";
  endpoint_name: string = "";
  url: string = "";
  tier: number = 0;
  host?: string;
  tags: ServerTag[] = [];
  remark?: string;

  constructor(options: Server) {
    Object.assign(this, options);
  }
}

export const ServerList: Server[] = [];

const _add = (options: Server) => ServerList.push(new Server(options));

_add({
  id: "fc40861b-eea2-4177-b3b3-d1bb8e1bb431",
  endpoint_name: "debian",
  url: "http://deb.debian.org/debian/",
  tier: 1,
  host: "",
  tags: [ServerTag.Origin],
  remark: "Debian Official Repository",
});

_add({
  id: "8fb3bfcf-c71f-4287-8ffe-8732b3b355c6",
  endpoint_name: "ubuntu",
  url: "https://mirrors.aliyun.com/ubuntu/",
  tier: 2,
  host: "",
  tags: [],
  remark: "Aliyun Mirrors - /ubuntu/",
});

_add({
  id: "af62f989-666b-4dd0-818e-5d782e3bd5be",
  endpoint_name: "ubuntu",
  url: "http://archive.ubuntu.com/ubuntu/",
  tier: 2,
  host: "",
  tags: [],
  remark: "Ubuntu Official Repository",
});
