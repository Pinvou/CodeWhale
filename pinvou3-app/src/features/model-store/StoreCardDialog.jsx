// 商店卡片详情:这张卡是什么、装它要付出多少、以及双机方案的额外前提。
//
// 双机那段刻意写得长:它是「这张卡能不能装」的前提,用户在点下载之前就必须知道
// 需要第二台设备和一根直连线,事后在部署阶段才发现等于白下几十 GB。
//
// 权重校验档位那条黄条同理,而且**这里比「我的模型」那个详情页更要紧**:它是下载
// 决策点。等下完几十 GB 再告诉用户"这份权重没跟官方摘要比对过",他已经付出了代价。
import { Download } from '../../components/icons.jsx';
import { formatBytes } from './model-store-logic.js';
import {
  MsButton,
  MsDialog,
  MsDialogTitle,
  MsPill,
  MsRow,
  TEXT_SECONDARY,
  WeightVerifyNotice,
} from './model-store-ui.jsx';

const TITLE_ID = 'model-store-card-title';

/**
 * 商店卡片的详情弹窗。
 * @returns {React.ReactElement|null} 弹窗。
 */
function StoreCardDialog({ card, copy, theme, onClose, onDownload, downloaded, downloading }) {
  const isDual = card?.deploy_topology === 'dual';
  return (
    <MsDialog open={!!card} onClose={onClose} theme={theme} labelledBy={TITLE_ID} testId="model-store-card-dialog">
      {card ? (
        <>
          <MsDialogTitle id={TITLE_ID} onClose={onClose} closeLabel={copy.detailClose}>
            {card.name}
          </MsDialogTitle>

          <div className="mb-4 flex flex-wrap gap-1.5">
            <MsPill tone="accent">
              {card.engine_name}
              {card.engine_version ? ` ${card.engine_version}` : ''}
            </MsPill>
            {card.quantization ? <MsPill>{card.quantization}</MsPill> : null}
            <MsPill tone={isDual ? 'warn' : 'neutral'}>{isDual ? copy.topologyDual : copy.topologySingle}</MsPill>
          </div>

          <div className="space-y-1">
            {card.org ? <MsRow label={copy.detailProvider}>{card.org}</MsRow> : null}
            <MsRow label={copy.detailEngine}>
              {card.engine_name}
              {card.engine_version ? ` ${card.engine_version}` : ''}
            </MsRow>
            {card.quantization ? <MsRow label={copy.detailQuantization}>{card.quantization}</MsRow> : null}
            <MsRow label={copy.detailSize}>
              {formatBytes(card.size_bytes)}
              {isDual ? copy.detailSizeDual : ''}
            </MsRow>
          </div>

          {isDual ? (
            <div className="mt-4 space-y-1.5 rounded-[14px] bg-[#FEF7E0] px-3 py-2.5 text-[12px] leading-5 text-[#A15C07] dark:bg-[#3D3323] dark:text-[#FDD663]">
              <p>{copy.dualNotice}</p>
              <p>{copy.dualNoticeDownload}</p>
              <p>{copy.dualNoticeDeploy}</p>
            </div>
          ) : null}

          <WeightVerifyNotice card={card} copy={copy} />

          {card.description ? (
            <p className={`mt-4 whitespace-pre-wrap text-[12px] leading-5 ${TEXT_SECONDARY}`}>{card.description}</p>
          ) : null}

          <div className="mt-6 flex justify-end gap-2">
            <MsButton tone="ghost" onClick={onClose}>{copy.detailClose}</MsButton>
            <MsButton
              disabled={downloaded || downloading}
              busy={downloading}
              onClick={() => onDownload(card)}
              data-testid="model-store-card-dialog-download"
            >
              {downloading ? null : <Download size={14} />}
              {downloaded ? copy.downloaded : (downloading ? copy.downloading : copy.download)}
            </MsButton>
          </div>
        </>
      ) : null}
    </MsDialog>
  );
}

export { StoreCardDialog };
