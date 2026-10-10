import { useState } from 'react';
import { ChevronRight, Sparkles } from '../../components/icons.jsx';
import { localizeTool, tsToolsData, tsToolWelcomeData } from './tool-marketplace.jsx';

const ToolWelcomeCard = ({ toolId, theme, t, onSend }) => {
      const [hovered, setHovered] = useState(null);
      // 组合包化的本地能力(pptx)已无商店连接器卡,欢迎卡数据回退 tsToolWelcomeData
      const tool = localizeTool(tsToolsData.find(item => item.backendId === toolId) || tsToolWelcomeData.find(item => item.backendId === toolId), t);
      if (!tool || !tool.welcomeQueries) return null;
      const ToolIcon = tool.icon || Sparkles;
      return (
        <div className="flex justify-start">
          <div className={`max-w-[800px] w-full rounded-[2rem] overflow-hidden border transition-all ${
            'bg-white border-slate-100 shadow-lg shadow-slate-200/30 dark:bg-[#1E1F20] dark:border-[#3A3A3C]/60'
          }`}>
            <div className={`relative p-5 border-b flex items-center gap-3.5 ${
              'bg-gradient-to-b from-blue-50/80 to-white border-slate-100 dark:bg-[#1E1F20] dark:border-[#3A3A3C]/60'
            }`}>
              <div className="bg-gradient-to-tr from-blue-600 to-indigo-500 p-2.5 rounded-xl shadow-lg shadow-blue-500/30">
                <ToolIcon size={22} className="text-white" />
              </div>
              <div>
                <div className={`text-[1.05rem] font-bold tracking-tight ${'text-slate-800 dark:text-slate-100'}`}>{tool.title}</div>
                <div className={`flex items-center text-xs mt-0.5 ${'text-slate-500 dark:text-slate-400'}`}>
                  <span className="relative flex h-2 w-2 mr-2">
                    <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-emerald-400 opacity-75"></span>
                    <span className="relative inline-flex rounded-full h-2 w-2 bg-emerald-500"></span>
                  </span>
                  {/* The install path deliberately keeps the switch off (DenyAll
                      convergence), so "Ready" would be a lie; describe it truthfully
                      and complete the opt-in on the first question (review #455 R7-M4). */}
                  {t.uiChat.installedReady}
                </div>
              </div>
            </div>
            <div className="p-5">
              <p className={`leading-relaxed text-[15px] ${'text-slate-600 dark:text-slate-300'}`}>
                {tool.desc.split('。')[0]}。{t.uiChat.naturalQuestion}
              </p>
              <div className="flex items-center my-5">
                <div className={`flex-grow h-px ${'bg-gradient-to-r from-transparent via-slate-200 to-transparent dark:bg-gradient-to-r dark:from-transparent dark:via-[#3A3A3C] dark:to-transparent'}`}></div>
                <span className={`px-4 text-[11px] uppercase tracking-wider font-semibold flex items-center gap-1.5 ${'text-slate-400 dark:text-slate-500'}`}>
                  <Sparkles size={13} />
                  <span>{t.uiChat.askMe}</span>
                </span>
                <div className={`flex-grow h-px ${'bg-gradient-to-r from-transparent via-slate-200 to-transparent dark:bg-gradient-to-r dark:from-transparent dark:via-[#3A3A3C] dark:to-transparent'}`}></div>
              </div>
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-2.5">
                {tool.welcomeQueries.map((q, i) => (
                  <button
                    key={i}
                    onMouseEnter={() => setHovered(i)}
                    onMouseLeave={() => setHovered(null)}
                    onClick={() => onSend && onSend(q)}
                    className={`group relative flex items-center justify-between p-3 rounded-2xl border text-left transition-all duration-200 ${
                      hovered === i
                        ? 'border-blue-200/80 bg-blue-50/50 shadow-sm dark:border-blue-500/30 dark:bg-blue-500/10'
                        : 'border-slate-200/60 bg-slate-50/30 hover:border-blue-200 dark:border-[#3A3A3C]/50 dark:bg-[#2A2B2D]/30 dark:hover:border-[#555]'
                    }`}
                  >
                    <span className={`text-sm font-medium transition-colors ${
                      hovered === i ? 'text-blue-700 dark:text-blue-300' : 'text-slate-700 dark:text-slate-300'
                    }`}>{q}</span>
                    <ChevronRight size={15} className={`transition-all duration-200 ${
                      hovered === i ? 'text-blue-500 opacity-100 dark:text-blue-400' : 'opacity-0 -translate-x-2'
                    }`} />
                  </button>
                ))}
              </div>
            </div>
          </div>
        </div>
      );
    };

export default ToolWelcomeCard;
